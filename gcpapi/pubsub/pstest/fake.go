package pstest

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "google.golang.org/genproto/googleapis/pubsub/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	durpb "google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Khan/districts-jobs/pkg/gcpapi/testutil"
)

// ReactorOptions is a map that Server uses to look up reactors.
// Key is the function name, value is array of reactor for the function.
type ReactorOptions map[string][]Reactor

// Reactor is an interface to allow reaction function to a certain call.
type Reactor interface {
	// React handles the message types and returns results.  If "handled" is false,
	// then the test server will ignore the results and continue to the next reactor
	// or the original handler.
	React(_ interface{}) (handled bool, ret interface{}, err error)
}

// ServerReactorOption is options passed to the server for reactor creation.
type ServerReactorOption struct {
	Reactor  Reactor
	FuncName string
}

// For testing. Note that even though changes to the now variable are atomic, a call
// to the stored function can race with a change to that function. This could be a
// problem if tests are run in parallel, or even if concurrent parts of the same test
// change the value of the variable.
var now atomic.Value

func init() {
	now.Store(time.Now)
	ResetMinAckDeadline()
}

func timeNow() time.Time {
	return now.Load().(func() time.Time)()
}

// Server is a fake Pub/Sub server.
type Server struct {
	GServer GServer
	srv     *testutil.Server
	Addr    string
}

// GServer is the underlying service implementor. It is not intended to be used
// directly.
type GServer struct {
	pb.PublisherServer
	pb.SubscriberServer
	topics         map[string]*topic
	subs           map[string]*subscription
	reactorOptions ReactorOptions
	msgsByID       map[string]*Message
	timeNowFunc    func() time.Time
	msgs           []*Message
	nextID         int
	streamTimeout  time.Duration
	wg             sync.WaitGroup
	mu             sync.Mutex
}

// NewServer creates a new fake server running in the current process.
func NewServer(opts ...ServerReactorOption) *Server {
	srv, err := testutil.NewServer()
	if err != nil {
		panic(fmt.Sprintf("pstest.NewServer: %v", err))
	}
	reactorOptions := ReactorOptions{}
	for _, opt := range opts {
		reactorOptions[opt.FuncName] = append(reactorOptions[opt.FuncName], opt.Reactor)
	}
	s := &Server{
		srv:  srv,
		Addr: srv.Addr,
		GServer: GServer{
			topics:         map[string]*topic{},
			subs:           map[string]*subscription{},
			msgsByID:       map[string]*Message{},
			timeNowFunc:    timeNow,
			reactorOptions: reactorOptions,
		},
	}
	pb.RegisterPublisherServer(srv.Gsrv, &s.GServer)
	pb.RegisterSubscriberServer(srv.Gsrv, &s.GServer)
	srv.Start()
	return s
}

// SetTimeNowFunc registers f as a function to
// be used instead of time.Now for this server.
func (s *Server) SetTimeNowFunc(f func() time.Time) {
	s.GServer.timeNowFunc = f
}

// Publish behaves as if the Publish RPC was called with a message with the given
// data and attrs. It returns the ID of the message.
// The topic will be created if it doesn't exist.
//
// Publish panics if there is an error, which is appropriate for testing.
func (s *Server) Publish(topic string, data []byte, attrs map[string]string) string {
	return s.PublishOrdered(topic, data, attrs, "")
}

// PublishOrdered behaves as if the Publish RPC was called with a message with the given
// data, attrs and ordering key. It returns the ID of the message.
// The topic will be created if it doesn't exist.
//
// PublishOrdered panics if there is an error, which is appropriate for testing.
func (s *Server) PublishOrdered(
	topic string,
	data []byte,
	attrs map[string]string,
	orderingKey string,
) string {
	const topicPattern = "projects/*/topics/*"
	ok, err := path.Match(topicPattern, topic)
	if err != nil {
		panic(err)
	}
	if !ok {
		panic(fmt.Sprintf("topic name must be of the form %q", topicPattern))
	}
	_, _ = s.GServer.CreateTopic(context.TODO(), &pb.Topic{Name: topic})
	req := &pb.PublishRequest{
		Topic:    topic,
		Messages: []*pb.PubsubMessage{{Data: data, Attributes: attrs, OrderingKey: orderingKey}},
	}
	res, err := s.GServer.Publish(context.TODO(), req)
	if err != nil {
		panic(fmt.Sprintf("pstest.Server.Publish: %v", err))
	}
	return res.MessageIds[0]
}

// SetStreamTimeout sets the amount of time a stream will be active before it shuts
// itself down. This mimics the real service's behavior of closing streams after 30
// minutes. If SetStreamTimeout is never called or is passed zero, streams never shut
// down.
func (s *Server) SetStreamTimeout(d time.Duration) {
	s.GServer.mu.Lock()
	defer s.GServer.mu.Unlock()
	s.GServer.streamTimeout = d
}

// A Message is a message that was published to the server.
type Message struct {
	PublishTime time.Time
	Attributes  map[string]string
	ID          string
	OrderingKey string
	modacks     []Modack
	Modacks     []Modack
	Data        []byte
	Acks        int
	deliveries  int
	acks        int
	Deliveries  int
}

// Modack represents a modack sent to the server.
type Modack struct {
	ReceivedAt  time.Time
	AckID       string
	AckDeadline int32
}

// Messages returns information about all messages ever published.
func (s *Server) Messages() []*Message {
	s.GServer.mu.Lock()
	defer s.GServer.mu.Unlock()

	var msgs []*Message
	for _, m := range s.GServer.msgs {
		m.Deliveries = m.deliveries
		m.Acks = m.acks
		m.Modacks = append([]Modack(nil), m.modacks...)
		msgs = append(msgs, m)
	}
	return msgs
}

// Message returns the message with the given ID, or nil if no message
// with that ID was published.
func (s *Server) Message(id string) *Message {
	s.GServer.mu.Lock()
	defer s.GServer.mu.Unlock()

	m := s.GServer.msgsByID[id]
	if m != nil {
		m.Deliveries = m.deliveries
		m.Acks = m.acks
		m.Modacks = append([]Modack(nil), m.modacks...)
	}
	return m
}

// Wait blocks until all server activity has completed.
func (s *Server) Wait() {
	s.GServer.wg.Wait()
}

// ClearMessages removes all published messages
// from internal containers.
func (s *Server) ClearMessages() {
	s.GServer.mu.Lock()
	s.GServer.msgs = nil
	s.GServer.msgsByID = make(map[string]*Message)
	s.GServer.mu.Unlock()
}

// Close shuts down the server and releases all resources.
func (s *Server) Close() error {
	s.srv.Close()
	s.GServer.mu.Lock()
	defer s.GServer.mu.Unlock()
	for _, sub := range s.GServer.subs {
		sub.stop()
	}
	return nil
}

func (s *GServer) CreateTopic(_ context.Context, t *pb.Topic) (*pb.Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(t, "CreateTopic", &pb.Topic{}); handled || err != nil {
		return ret.(*pb.Topic), err
	}

	if s.topics[t.Name] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "topic %q", t.Name)
	}
	top := newTopic(t)
	s.topics[t.Name] = top
	return top.proto, nil
}

func (s *GServer) GetTopic(_ context.Context, req *pb.GetTopicRequest) (*pb.Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "GetTopic", &pb.Topic{}); handled || err != nil {
		return ret.(*pb.Topic), err
	}

	if t := s.topics[req.Topic]; t != nil {
		return t.proto, nil
	}
	return nil, status.Errorf(codes.NotFound, "topic %q", req.Topic)
}

func (s *GServer) UpdateTopic(_ context.Context, req *pb.UpdateTopicRequest) (*pb.Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "UpdateTopic", &pb.Topic{}); handled || err != nil {
		return ret.(*pb.Topic), err
	}

	t := s.topics[req.Topic.Name]
	if t == nil {
		return nil, status.Errorf(codes.NotFound, "topic %q", req.Topic.Name)
	}
	for _, maskPath := range req.UpdateMask.Paths {
		switch maskPath {
		case "labels":
			t.proto.Labels = req.Topic.Labels
		case "message_storage_policy":
			t.proto.MessageStoragePolicy = req.Topic.MessageStoragePolicy
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown field name %q", maskPath)
		}
	}
	return t.proto, nil
}

func (s *GServer) ListTopics(
	_ context.Context,
	req *pb.ListTopicsRequest,
) (*pb.ListTopicsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "ListTopics", &pb.ListTopicsResponse{}); handled ||
		err != nil {
		return ret.(*pb.ListTopicsResponse), err
	}

	var names []string
	for n := range s.topics {
		if strings.HasPrefix(n, req.Project) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	from, to, nextToken, err := testutil.PageBounds(int(req.PageSize), req.PageToken, len(names))
	if err != nil {
		return nil, err
	}
	res := &pb.ListTopicsResponse{NextPageToken: nextToken}
	for i := from; i < to; i++ {
		res.Topics = append(res.Topics, s.topics[names[i]].proto)
	}
	return res, nil
}

func (s *GServer) ListTopicSubscriptions(
	_ context.Context,
	req *pb.ListTopicSubscriptionsRequest,
) (*pb.ListTopicSubscriptionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "ListTopicSubscriptions", &pb.ListTopicSubscriptionsResponse{}); handled ||
		err != nil {
		return ret.(*pb.ListTopicSubscriptionsResponse), err
	}

	var names []string
	for name, sub := range s.subs {
		if sub.topic.proto.Name == req.Topic {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	from, to, nextToken, err := testutil.PageBounds(int(req.PageSize), req.PageToken, len(names))
	if err != nil {
		return nil, err
	}
	return &pb.ListTopicSubscriptionsResponse{
		Subscriptions: names[from:to],
		NextPageToken: nextToken,
	}, nil
}

func (s *GServer) DeleteTopic(
	_ context.Context,
	req *pb.DeleteTopicRequest,
) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "DeleteTopic", &emptypb.Empty{}); handled ||
		err != nil {
		return ret.(*emptypb.Empty), err
	}

	t := s.topics[req.Topic]
	if t == nil {
		return nil, status.Errorf(codes.NotFound, "topic %q", req.Topic)
	}
	t.stop()
	delete(s.topics, req.Topic)
	return &emptypb.Empty{}, nil
}

func (s *GServer) CreateSubscription(
	_ context.Context,
	ps *pb.Subscription,
) (*pb.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(ps, "CreateSubscription", &pb.Subscription{}); handled ||
		err != nil {
		return ret.(*pb.Subscription), err
	}

	if ps.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing name")
	}
	if s.subs[ps.Name] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "subscription %q", ps.Name)
	}
	if ps.Topic == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing topic")
	}
	top := s.topics[ps.Topic]
	if top == nil {
		return nil, status.Errorf(codes.NotFound, "topic %q", ps.Topic)
	}
	if err := checkAckDeadline(ps.AckDeadlineSeconds); err != nil {
		return nil, err
	}
	if ps.MessageRetentionDuration == nil {
		ps.MessageRetentionDuration = defaultMessageRetentionDuration
	}
	if err := checkMRD(ps.MessageRetentionDuration); err != nil {
		return nil, err
	}
	if ps.PushConfig == nil {
		ps.PushConfig = &pb.PushConfig{}
	}

	sub := newSubscription(top, &s.mu, s.timeNowFunc, ps)
	top.subs[ps.Name] = sub
	s.subs[ps.Name] = sub
	sub.start(&s.wg)
	return ps, nil
}

// Can be set for testing.
var minAckDeadlineSecs int32

// SetMinAckDeadline changes the minack deadline to n. Must be
// greater than or equal to 1 second. Remember to reset this value
// to the default after your test changes it. Example usage:
// 		pstest.SetMinAckDeadlineSecs(1)
// 		defer pstest.ResetMinAckDeadlineSecs()
func SetMinAckDeadline(n time.Duration) {
	if n < time.Second {
		panic("SetMinAckDeadline expects a value greater than 1 second")
	}

	minAckDeadlineSecs = int32(n / time.Second)
}

// ResetMinAckDeadline resets the minack deadline to the default.
func ResetMinAckDeadline() {
	minAckDeadlineSecs = 10
}

func checkAckDeadline(ads int32) error {
	if ads < minAckDeadlineSecs || ads > 600 {
		// PubSub service returns Unknown.
		return status.Errorf(codes.Unknown, "bad ack_deadline_seconds: %d", ads)
	}
	return nil
}

const (
	minMessageRetentionDuration = 10 * time.Minute
	maxMessageRetentionDuration = 168 * time.Hour
)

var defaultMessageRetentionDuration = durpb.New(maxMessageRetentionDuration)

func checkMRD(pmrd *durpb.Duration) error {
	mrd := pmrd.AsDuration()
	if mrd < minMessageRetentionDuration || mrd > maxMessageRetentionDuration {
		return status.Errorf(codes.InvalidArgument, "bad message_retention_duration %+v", pmrd)
	}
	return nil
}

func (s *GServer) GetSubscription(
	_ context.Context,
	req *pb.GetSubscriptionRequest,
) (*pb.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "GetSubscription", &pb.Subscription{}); handled ||
		err != nil {
		return ret.(*pb.Subscription), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	return sub.proto, nil
}

func (s *GServer) UpdateSubscription(
	_ context.Context,
	req *pb.UpdateSubscriptionRequest,
) (*pb.Subscription, error) {
	if req.Subscription == nil {
		return nil, status.Errorf(codes.InvalidArgument, "missing subscription")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "UpdateSubscription", &pb.Subscription{}); handled ||
		err != nil {
		return ret.(*pb.Subscription), err
	}

	sub, err := s.findSubscription(req.Subscription.Name)
	if err != nil {
		return nil, err
	}
	for _, maskPath := range req.UpdateMask.Paths {
		switch maskPath {
		case "push_config":
			sub.proto.PushConfig = req.Subscription.PushConfig

		case "ack_deadline_seconds":
			a := req.Subscription.AckDeadlineSeconds
			if err := checkAckDeadline(a); err != nil {
				return nil, err
			}
			sub.proto.AckDeadlineSeconds = a

		case "retain_acked_messages":
			sub.proto.RetainAckedMessages = req.Subscription.RetainAckedMessages

		case "message_retention_duration":
			if err := checkMRD(req.Subscription.MessageRetentionDuration); err != nil {
				return nil, err
			}
			sub.proto.MessageRetentionDuration = req.Subscription.MessageRetentionDuration

		case "labels":
			sub.proto.Labels = req.Subscription.Labels

		case "expiration_policy":
			sub.proto.ExpirationPolicy = req.Subscription.ExpirationPolicy

		case "dead_letter_policy":
			sub.proto.DeadLetterPolicy = req.Subscription.DeadLetterPolicy

		case "retry_policy":
			sub.proto.RetryPolicy = req.Subscription.RetryPolicy

		case "filter":
			sub.proto.Filter = req.Subscription.Filter

		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown field name %q", maskPath)
		}
	}
	return sub.proto, nil
}

func (s *GServer) ListSubscriptions(
	_ context.Context,
	req *pb.ListSubscriptionsRequest,
) (*pb.ListSubscriptionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "ListSubscriptions", &pb.ListSubscriptionsResponse{}); handled ||
		err != nil {
		return ret.(*pb.ListSubscriptionsResponse), err
	}

	var names []string
	for name := range s.subs {
		if strings.HasPrefix(name, req.Project) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	from, to, nextToken, err := testutil.PageBounds(int(req.PageSize), req.PageToken, len(names))
	if err != nil {
		return nil, err
	}
	res := &pb.ListSubscriptionsResponse{NextPageToken: nextToken}
	for i := from; i < to; i++ {
		res.Subscriptions = append(res.Subscriptions, s.subs[names[i]].proto)
	}
	return res, nil
}

func (s *GServer) DeleteSubscription(
	_ context.Context,
	req *pb.DeleteSubscriptionRequest,
) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "DeleteSubscription", &emptypb.Empty{}); handled ||
		err != nil {
		return ret.(*emptypb.Empty), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	sub.stop()
	delete(s.subs, req.Subscription)
	sub.topic.deleteSub(sub)
	return &emptypb.Empty{}, nil
}

func (s *GServer) DetachSubscription(
	_ context.Context,
	req *pb.DetachSubscriptionRequest,
) (*pb.DetachSubscriptionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "DetachSubscription", &pb.DetachSubscriptionResponse{}); handled ||
		err != nil {
		return ret.(*pb.DetachSubscriptionResponse), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	sub.topic.deleteSub(sub)
	return &pb.DetachSubscriptionResponse{}, nil
}

func (s *GServer) Publish(_ context.Context, req *pb.PublishRequest) (*pb.PublishResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "Publish", &pb.PublishResponse{}); handled ||
		err != nil {
		return ret.(*pb.PublishResponse), err
	}

	if req.Topic == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing topic")
	}
	top := s.topics[req.Topic]
	if top == nil {
		return nil, status.Errorf(codes.NotFound, "topic %q", req.Topic)
	}
	var ids []string
	for _, pm := range req.Messages {
		id := fmt.Sprintf("m%d", s.nextID)
		s.nextID++
		pm.MessageId = id
		pubTime := s.timeNowFunc()
		tsPubTime := timestamppb.New(pubTime)
		pm.PublishTime = tsPubTime
		m := &Message{
			ID:          id,
			Data:        pm.Data,
			Attributes:  pm.Attributes,
			PublishTime: pubTime,
			OrderingKey: pm.OrderingKey,
		}
		top.publish(pm, m)
		ids = append(ids, id)
		s.msgs = append(s.msgs, m)
		s.msgsByID[id] = m
	}
	return &pb.PublishResponse{MessageIds: ids}, nil
}

type topic struct {
	proto *pb.Topic
	subs  map[string]*subscription
}

func newTopic(pt *pb.Topic) *topic {
	return &topic{
		proto: pt,
		subs:  map[string]*subscription{},
	}
}

func (t *topic) stop() {
	for _, sub := range t.subs {
		sub.proto.Topic = "_deleted-topic_"
	}
}

func (t *topic) deleteSub(sub *subscription) {
	delete(t.subs, sub.proto.Name)
}

func (t *topic) publish(pm *pb.PubsubMessage, m *Message) {
	for _, s := range t.subs {
		s.msgs[pm.MessageId] = &message{
			publishTime: m.PublishTime,
			proto: &pb.ReceivedMessage{
				AckId:   pm.MessageId,
				Message: pm,
			},
			deliveries:  &m.deliveries,
			acks:        &m.acks,
			streamIndex: -1,
		}
	}
}

type subscription struct {
	topic       *topic
	mu          *sync.Mutex
	proto       *pb.Subscription
	msgs        map[string]*message
	done        chan struct{}
	timeNowFunc func() time.Time
	streams     []*stream
	ackTimeout  time.Duration
}

func newSubscription(
	t *topic,
	mu *sync.Mutex,
	timeNowFunc func() time.Time,
	ps *pb.Subscription,
) *subscription {
	at := time.Duration(ps.AckDeadlineSeconds) * time.Second
	if at == 0 {
		at = 10 * time.Second
	}
	return &subscription{
		topic:       t,
		mu:          mu,
		proto:       ps,
		ackTimeout:  at,
		msgs:        map[string]*message{},
		done:        make(chan struct{}),
		timeNowFunc: timeNowFunc,
	}
}

func (s *subscription) start(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-s.done:
				return
			case <-time.After(10 * time.Millisecond):
				s.deliver()
			}
		}
	}()
}

func (s *subscription) stop() {
	close(s.done)
}

func (s *GServer) Acknowledge(
	_ context.Context,
	req *pb.AcknowledgeRequest,
) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "Acknowledge", &emptypb.Empty{}); handled ||
		err != nil {
		return ret.(*emptypb.Empty), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	for _, id := range req.AckIds {
		sub.ack(id)
	}
	return &emptypb.Empty{}, nil
}

func (s *GServer) ModifyAckDeadline(
	_ context.Context,
	req *pb.ModifyAckDeadlineRequest,
) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "ModifyAckDeadline", &emptypb.Empty{}); handled ||
		err != nil {
		return ret.(*emptypb.Empty), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, id := range req.AckIds {
		s.msgsByID[id].modacks = append(
			s.msgsByID[id].modacks,
			Modack{AckID: id, AckDeadline: req.AckDeadlineSeconds, ReceivedAt: now},
		)
	}
	dur := secsToDur(req.AckDeadlineSeconds)
	for _, id := range req.AckIds {
		sub.modifyAckDeadline(id, dur)
	}
	return &emptypb.Empty{}, nil
}

func (s *GServer) Pull(ctx context.Context, req *pb.PullRequest) (*pb.PullResponse, error) {
	s.mu.Lock()

	if handled, ret, err := s.runReactor(req, "Pull", &pb.PullResponse{}); handled || err != nil {
		s.mu.Unlock()
		return ret.(*pb.PullResponse), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	max := int(req.MaxMessages)
	if max < 0 {
		s.mu.Unlock()
		return nil, status.Error(codes.InvalidArgument, "MaxMessages cannot be negative")
	}
	if max == 0 { // MaxMessages not specified; use a default.
		max = 1000
	}
	msgs := sub.pull(max)
	s.mu.Unlock()
	// Implement the spec from the pubsub proto:
	// "If ReturnImmediately set to true, the system will respond immediately even if
	// it there are no messages available to return in the `Pull` response.
	// Otherwise, the system may wait (for a bounded amount of time) until at
	// least one message is available, rather than returning no messages."
	if len(msgs) == 0 && !req.ReturnImmediately {
		// Wait for a short amount of time for a message.
		// TODO: signal when a message arrives, so we don't wait the whole time.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			s.mu.Lock()
			msgs = sub.pull(max)
			s.mu.Unlock()
		}
	}
	return &pb.PullResponse{ReceivedMessages: msgs}, nil
}

func (s *GServer) StreamingPull(sps pb.Subscriber_StreamingPullServer) error {
	// Receive initial message configuring the pull.
	req, err := sps.Recv()
	if err != nil {
		return err
	}
	s.mu.Lock()
	sub, err := s.findSubscription(req.Subscription)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// Create a new stream to handle the pull.
	st := sub.newStream(sps, s.streamTimeout)
	err = st.pull(&s.wg)
	sub.deleteStream(st)
	return err
}

func (s *GServer) Seek(ctx context.Context, req *pb.SeekRequest) (*pb.SeekResponse, error) {
	// Only handle time-based seeking for now.
	// This fake doesn't deal with snapshots.
	var target time.Time
	switch v := req.Target.(type) {
	case nil:
		return nil, status.Errorf(codes.InvalidArgument, "missing Seek target type")
	case *pb.SeekRequest_Time:
		target = v.Time.AsTime()
	default:
		return nil, status.Errorf(codes.Unimplemented, "unhandled Seek target type %T", v)
	}

	// The entire server must be locked while doing the work below,
	// because the messages don't have any other synchronization.
	s.mu.Lock()
	defer s.mu.Unlock()

	if handled, ret, err := s.runReactor(req, "Seek", &pb.SeekResponse{}); handled || err != nil {
		return ret.(*pb.SeekResponse), err
	}

	sub, err := s.findSubscription(req.Subscription)
	if err != nil {
		return nil, err
	}
	// Drop all messages from sub that were published before the target time.
	for id, m := range sub.msgs {
		if m.publishTime.Before(target) {
			delete(sub.msgs, id)
			(*m.acks)++
		}
	}
	// Un-ack any already-acked messages after this time;
	// redelivering them to the subscription is the closest analogue here.
	for _, m := range s.msgs {
		if m.PublishTime.Before(target) {
			continue
		}
		sub.msgs[m.ID] = &message{
			publishTime: m.PublishTime,
			proto: &pb.ReceivedMessage{
				AckId: m.ID,
				// This was not preserved!
				// Message: pm,
			},
			deliveries:  &m.deliveries,
			acks:        &m.acks,
			streamIndex: -1,
		}
	}
	return &pb.SeekResponse{}, nil
}

// Gets a subscription that must exist.
// Must be called with the lock held.
func (s *GServer) findSubscription(name string) (*subscription, error) {
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing subscription")
	}
	sub := s.subs[name]
	if sub == nil {
		return nil, status.Errorf(codes.NotFound, "subscription %s", name)
	}
	return sub, nil
}

// Must be called with the lock held.
func (s *subscription) pull(max int) []*pb.ReceivedMessage {
	now := s.timeNowFunc()
	s.maintainMessages(now)
	var msgs []*pb.ReceivedMessage
	for _, m := range s.msgs {
		if m.outstanding() {
			continue
		}
		(*m.deliveries)++
		m.ackDeadline = now.Add(s.ackTimeout)
		msgs = append(msgs, m.proto)
		if len(msgs) >= max {
			break
		}
	}
	return msgs
}

func (s *subscription) deliver() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.timeNowFunc()
	s.maintainMessages(now)
	// Try to deliver each remaining message.
	curIndex := 0
	for _, m := range s.msgs {
		if m.outstanding() {
			continue
		}
		// If the message was never delivered before, start with the stream at
		// curIndex. If it was delivered before, start with the stream after the one
		// that owned it.
		if m.streamIndex < 0 {
			delIndex, ok := s.tryDeliverMessage(m, curIndex, now)
			if !ok {
				break
			}
			curIndex = delIndex + 1
			m.streamIndex = curIndex
		} else {
			delIndex, ok := s.tryDeliverMessage(m, m.streamIndex, now)
			if !ok {
				break
			}
			m.streamIndex = delIndex
		}
	}
}

// tryDeliverMessage attempts to deliver m to the stream at index i. If it can't, it
// tries streams i+1, i+2, ..., wrapping around. Once it's tried all streams, it
// exits.
//
// It returns the index of the stream it delivered the message to, or 0, false if
// it didn't deliver the message.
//
// Must be called with the lock held.
func (s *subscription) tryDeliverMessage(m *message, start int, now time.Time) (int, bool) {
	for i := 0; i < len(s.streams); i++ {
		idx := (i + start) % len(s.streams)

		st := s.streams[idx]
		select {
		case <-st.done:
			s.streams = deleteStreamAt(s.streams, idx)
			i--

		case st.msgc <- m.proto:
			(*m.deliveries)++
			m.ackDeadline = now.Add(st.ackTimeout)
			return idx, true

		default:
		}
	}
	return 0, false
}

var retentionDuration = 10 * time.Minute

// Must be called with the lock held.
func (s *subscription) maintainMessages(now time.Time) {
	for id, m := range s.msgs {
		// Mark a message as re-deliverable if its ack deadline has expired.
		if m.outstanding() && now.After(m.ackDeadline) {
			m.makeAvailable()
		}
		pubTime := m.proto.Message.PublishTime.AsTime()
		// Remove messages that have been undelivered for a long time.
		if !m.outstanding() && now.Sub(pubTime) > retentionDuration {
			delete(s.msgs, id)
		}
	}
}

func (s *subscription) newStream(
	gs pb.Subscriber_StreamingPullServer,
	timeout time.Duration,
) *stream {
	st := &stream{
		sub:        s,
		done:       make(chan struct{}),
		msgc:       make(chan *pb.ReceivedMessage),
		gstream:    gs,
		ackTimeout: s.ackTimeout,
		timeout:    timeout,
	}
	s.mu.Lock()
	s.streams = append(s.streams, st)
	s.mu.Unlock()
	return st
}

func (s *subscription) deleteStream(st *stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var i int
	for i = 0; i < len(s.streams); i++ {
		if s.streams[i] == st {
			break
		}
	}
	if i < len(s.streams) {
		s.streams = deleteStreamAt(s.streams, i)
	}
}

func deleteStreamAt(s []*stream, i int) []*stream {
	// Preserve order for round-robin delivery.
	return append(s[:i], s[i+1:]...)
}

type message struct {
	proto       *pb.ReceivedMessage
	publishTime time.Time
	ackDeadline time.Time
	deliveries  *int
	acks        *int
	streamIndex int // index of stream that currently owns msg, for round-robin delivery
}

// A message is outstanding if it is owned by some stream.
func (m *message) outstanding() bool {
	return !m.ackDeadline.IsZero()
}

func (m *message) makeAvailable() {
	m.ackDeadline = time.Time{}
}

type stream struct {
	sub        *subscription
	done       chan struct{} // closed when the stream is finished
	msgc       chan *pb.ReceivedMessage
	gstream    pb.Subscriber_StreamingPullServer
	ackTimeout time.Duration
	timeout    time.Duration
}

// pull manages the StreamingPull interaction for the life of the stream.
func (st *stream) pull(wg *sync.WaitGroup) error {
	errc := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errc <- st.sendLoop()
	}()
	go func() {
		defer wg.Done()
		errc <- st.recvLoop()
	}()
	var tchan <-chan time.Time
	if st.timeout > 0 {
		tchan = time.After(st.timeout)
	}
	// Wait until one of the goroutines returns an error, or we time out.
	var err error
	select {
	case err = <-errc:
		if err == io.EOF {
			err = nil
		}
	case <-tchan:
	}
	close(st.done) // stop the other goroutine
	return err
}

func (st *stream) sendLoop() error {
	for {
		select {
		case <-st.done:
			return nil
		case rm := <-st.msgc:
			res := &pb.StreamingPullResponse{ReceivedMessages: []*pb.ReceivedMessage{rm}}
			if err := st.gstream.Send(res); err != nil {
				return err
			}
		}
	}
}

func (st *stream) recvLoop() error {
	for {
		req, err := st.gstream.Recv()
		if err != nil {
			return err
		}
		st.sub.handleStreamingPullRequest(st, req)
	}
}

func (s *subscription) handleStreamingPullRequest(st *stream, req *pb.StreamingPullRequest) {
	// Lock the entire server.
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, ackID := range req.AckIds {
		s.ack(ackID)
	}
	for i, id := range req.ModifyDeadlineAckIds {
		s.modifyAckDeadline(id, secsToDur(req.ModifyDeadlineSeconds[i]))
	}
	if req.StreamAckDeadlineSeconds > 0 {
		st.ackTimeout = secsToDur(req.StreamAckDeadlineSeconds)
	}
}

// Must be called with the lock held.
func (s *subscription) ack(id string) {
	m := s.msgs[id]
	if m != nil {
		(*m.acks)++
		delete(s.msgs, id)
	}
}

// Must be called with the lock held.
func (s *subscription) modifyAckDeadline(id string, d time.Duration) {
	m := s.msgs[id]
	if m == nil { // already acked: ignore.
		return
	}
	if d == 0 { // nack
		m.makeAvailable()
	} else { // extend the deadline by d
		m.ackDeadline = s.timeNowFunc().Add(d)
	}
}

func secsToDur(secs int32) time.Duration {
	return time.Duration(secs) * time.Second
}

// runReactor looks up the reactors for a function, then launches them until handled=true
// or err is returned. If the reactor returns nil, the function returns defaultObj instead.
func (s *GServer) runReactor(
	req interface{},
	funcName string,
	defaultObj interface{},
) (bool, interface{}, error) {
	if val, ok := s.reactorOptions[funcName]; ok {
		for _, reactor := range val {
			handled, ret, err := reactor.React(req)
			// If handled=true, that means the reactor has successfully reacted to the request,
			// so use the output directly. If err occurs, that means the request is invalidated
			// by the reactor somehow.
			if handled || err != nil {
				if ret == nil {
					ret = defaultObj
				}
				return true, ret, err
			}
		}
	}
	return false, nil, nil
}

// errorInjectionReactor is a reactor to inject an error message with status code.
type errorInjectionReactor struct {
	msg  string
	code codes.Code
}

// React simply returns an error with defined error message and status code.
func (e *errorInjectionReactor) React(_ interface{}) (handled bool, ret interface{}, err error) {
	return true, nil, status.Errorf(e.code, e.msg)
}

// WithErrorInjection creates a ServerReactorOption that injects error with defined status code and
// message for a certain function.
func WithErrorInjection(funcName string, code codes.Code, msg string) ServerReactorOption {
	return ServerReactorOption{
		FuncName: funcName,
		Reactor:  &errorInjectionReactor{code: code, msg: msg},
	}
}
