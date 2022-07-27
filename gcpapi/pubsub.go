package gcpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"

	"golang.org/x/sync/errgroup"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

type PubSubTopic string

type PubSubInfo struct {
	Client                *pubsub.Client
	SecretKey             string
	TopicCache            map[PubSubTopic]*pubsub.Topic
	TestServer            *pstest.Server
	SentMessageIDsByTopic map[PubSubTopic][]string
}

func NewPubSubInfoForTests(
	ctx context.Context,
	secretKey string,
	projectID string,
	options ...option.ClientOption,
) (*PubSubInfo, error) {
	client, err := pubsub.NewClient(ctx, projectID, options...)
	if err != nil {
		return nil, err
	}
	return &PubSubInfo{
		Client:                client,
		SecretKey:             secretKey,
		SentMessageIDsByTopic: map[PubSubTopic][]string{},
	}, nil
}

func NewPubSubInfo(
	ctx context.Context,
	secretKey string,
	projectID string,
	credentials []byte,
) (*PubSubInfo, error) {
	var err error
	var client *pubsub.Client

	if len(credentials) != 0 {
		client, err = pubsub.NewClient(
			ctx, projectID, option.WithCredentialsJSON(credentials))
	} else {
		client, err = pubsub.NewClient(ctx, projectID)
	}

	if err != nil {
		return nil, err
	}
	return &PubSubInfo{
		Client:                client,
		SecretKey:             secretKey,
		SentMessageIDsByTopic: map[PubSubTopic][]string{},
	}, nil
}

func (p *PubSubInfo) Close() {
	if p == nil {
		return
	}
	if p.Client != nil {
		p.Client.Close()
	}
	if p.TestServer != nil {
		p.TestServer.Close()
	}
}

// GetTopic pulls the topic from the saved map or gets it if it wasn't already
// in the map.  We don't want to call p.Client.Topic more than once if we don't
// need to.
func (p *PubSubInfo) GetTopic(topicStr PubSubTopic) *pubsub.Topic {
	if p.TopicCache == nil {
		p.TopicCache = map[PubSubTopic]*pubsub.Topic{}
	}
	topic, found := p.TopicCache[topicStr]
	if !found {
		topic = p.Client.Topic(string(topicStr))
		p.TopicCache[topicStr] = topic
	}
	return topic
}

func (p *PubSubInfo) SendPubSubMessage(
	ctx context.Context,
	topicStr PubSubTopic,
	message proto.Message,
) error {
	topic := p.GetTopic(topicStr)

	result, err := p.publishMessage(ctx, topic, message)
	if err != nil {
		return err
	}
	serverID, err := result.Get(ctx)
	p.SentMessageIDsByTopic[topicStr] = append(p.SentMessageIDsByTopic[topicStr], serverID)
	return err
}

func (p *PubSubInfo) publishMessage(
	ctx context.Context,
	topic *pubsub.Topic,
	message proto.Message,
) (*pubsub.PublishResult, error) {
	data, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	signature, err := p.ComputeSignatureWithSecret(data)
	if err != nil {
		return nil, err
	}

	result := topic.Publish(
		ctx,
		&pubsub.Message{
			Data: data,
			Attributes: map[string]string{
				"signature": signature,
			},
		},
	)
	return result, nil
}

const batchSize = 500

func (p *PubSubInfo) ClearTestMessages() {
	p.TestServer.ClearMessages()
	p.SentMessageIDsByTopic = map[PubSubTopic][]string{}
}

// SendPubSubMessages tries to send all of the
// Return the list of errors 1-1 for the messages
// and a boolean that returns true if there were any errors
func (p *PubSubInfo) SendPubSubMessages(
	ctx context.Context,
	topicStr PubSubTopic,
	messages []proto.Message,
) (errors []error, anyErrors bool) {
	numMessages := len(messages)
	errors = make([]error, numMessages)
	ids := make([]string, numMessages)
	if numMessages == 0 {
		return errors, true // nothing to do
	}
	topic := p.GetTopic(topicStr)

	start := 0
	for start < numMessages {
		stop := start + batchSize
		if stop > numMessages {
			stop = numMessages
		}
		eg, gtx := errgroup.WithContext(ctx)
		for i, message := range messages[start:stop] {
			index := i + start
			result, err := p.publishMessage(ctx, topic, message)
			if err != nil {
				errors[index] = err
				continue
			}
			eg.Go(func() error {
				serverID, err := result.Get(gtx)
				if err != nil {
					errors[index] = err
				}
				ids[index] = serverID
				return nil
			})
		}
		err := eg.Wait()
		if err != nil {
			// this is impossible!
			return errors, true
		}
		start = stop
	}

	for _, err := range errors {
		if err != nil {
			return errors, true
		}
	}
	p.SentMessageIDsByTopic[topicStr] = append(p.SentMessageIDsByTopic[topicStr], ids...)
	return errors, false
}

// ComputeSignatureWithSecret computes a signed hash given a message
// and a secret to sign with. This function should match the
// implementation in python to ensure interoperability.
func (p *PubSubInfo) ComputeSignatureWithSecret(msgBytes []byte) (string, error) {
	encodedMsg := base64.StdEncoding.EncodeToString(msgBytes)
	mac := hmac.New(sha512.New, []byte(p.SecretKey))
	_, err := mac.Write([]byte(encodedMsg))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
