// Package dsifake implements a fake Datastore
// per https://github.com/googleapis/google-cloud-go/blob/master/testing.md
// The crude key value store does not currently support transactions
package dsifake

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"cloud.google.com/go/datastore" //nolint:depguard // GKE ≠ AppEngine
	"google.golang.org/api/option"
	datastorepb "google.golang.org/genproto/googleapis/datastore/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// ErrNotImplemented is returned if a dsifake function is unimplemented.
// var ErrNotImplemented = errors.New("not implemented")

// FakeDatastore implements a crude datastore test client.  It is somewhat
// simplistic and incomplete.  It works only for basic Put, Get, and Delete,
// but may not always work correctly.
type FakeDatastore struct {
	datastorepb.UnimplementedDatastoreServer // For unimplemented methods
	lock                                     sync.Mutex
	objects                                  map[string][]byte
}

// NewClient returns a fake client that uses the FakeDatastore.
func NewClient(ctx context.Context) (*datastore.Client, *FakeDatastore) {
	cctx, cancel := context.WithCancel(ctx)
	// defer cancel()
	if flag.Lookup("test.v") == nil {
		log.Fatal("DSFakeClient should only be used in tests")
	}

	// Setup the fake server.
	fakeDatastore := &FakeDatastore{objects: make(map[string][]byte, 10)}
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	gsrv := grpc.NewServer()
	datastorepb.RegisterDatastoreServer(gsrv, fakeDatastore)
	fakeServerAddr := l.Addr().String()

	go func() {
		if err := gsrv.Serve(l); err != nil {
			panic(err)
		}
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("got signal %v, attempting graceful shutdown", s)
		cancel()
		gsrv.GracefulStop()
		// grpc.Stop() // leads to error while receiving stream response: rpc error: code =
		// Unavailable desc = transport is closing
	}()

	// Create a client.
	client, err := datastore.NewClient(cctx,
		"dsfake",
		option.WithEndpoint(fakeServerAddr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithInsecure()),
	)
	if err != nil {
		panic(err)
	}

	return client, fakeDatastore
}

// GetDSKeys lists all keys saved in the fake client.
func (c *FakeDatastore) GetDSKeys() []*datastore.Key {
	c.lock.Lock()
	defer c.lock.Unlock()
	keys := make([]*datastore.Key, len(c.objects))
	i := 0
	for _, v := range c.objects {
		var e datastorepb.Entity
		if err := proto.Unmarshal(v, &e); err != nil {
			continue
		}

		keys[i] = protoToKey(e.Key)
		i++
	}

	return keys
}

func (c *FakeDatastore) GetMap() map[string][]byte {
	c.lock.Lock()
	defer c.lock.Unlock()
	newMap := make(map[string][]byte, 10)
	for k, v := range c.objects {
		newMap[k] = v
	}
	return c.objects
}

// Commit - While this is a no-op, we need to satisfy the expectations for unmarshalling
func (c *FakeDatastore) Commit(
	_ context.Context,
	in *datastorepb.CommitRequest,
) (*datastorepb.CommitResponse, error) {
	keys := make([]*datastorepb.Key, 0, len(in.GetMutations()))
	c.lock.Lock()
	defer c.lock.Unlock()
	// c.OutputObjects()
	for _, v := range in.GetMutations() {
		switch op := v.GetOperation().(type) {
		case *datastorepb.Mutation_Update:
			pbKey := op.Update.Key

			_, ok := c.objects[protoKeyToKeyName(pbKey)]
			if ok {
				if b, marshalErr := proto.Marshal(op.Update); marshalErr == nil {
					keys = append(keys, pbKey)
					c.objects[protoKeyToKeyName(pbKey)] = b
				}
			}

		case *datastorepb.Mutation_Upsert:
			pbKey := op.Upsert.Key
			if b, err := proto.Marshal(op.Upsert); err == nil {
				keys = append(keys, pbKey)
				c.objects[protoKeyToKeyName(pbKey)] = b
			}

		case *datastorepb.Mutation_Delete:
			pbKey := op.Delete
			_, ok := c.objects[protoKeyToKeyName(pbKey)]
			if ok {
				keys = append(keys, op.Delete)
				delete(c.objects, protoKeyToKeyName(pbKey))
			}

		}
	}

	var mutationResults []*datastorepb.MutationResult
	for i := range keys {
		mutationResult := datastorepb.MutationResult{
			Key:              keys[i],
			Version:          0,
			ConflictDetected: false,
		}
		mutationResults = append(mutationResults, &mutationResult)
	}

	response := datastorepb.CommitResponse{
		MutationResults: mutationResults,
		IndexUpdates:    0,
	}
	// c.OutputObjects()
	return &response, nil
}

func (c *FakeDatastore) Lookup(
	_ context.Context,
	in *datastorepb.LookupRequest,
) (*datastorepb.LookupResponse, error) {
	pbKeys := in.GetKeys()
	found := make([]*datastorepb.EntityResult, 0, len(pbKeys))
	var missing []*datastorepb.EntityResult
	response := datastorepb.LookupResponse{
		Found:    nil,
		Missing:  nil,
		Deferred: nil,
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	// c.OutputObjects()

	for i := range pbKeys {
		v, ok := c.objects[protoKeyToKeyName(pbKeys[i])]
		if ok {
			var e datastorepb.Entity
			if err := proto.Unmarshal(v, &e); err != nil {
				missing = append(missing, entityResultFromKey(pbKeys[i]))
				continue
			}

			found = append(found, entityResultFromEntity(&e))
		} else {
			missing = append(missing, entityResultFromKey(pbKeys[i]))
		}
	}
	response.Found = found
	response.Missing = missing

	return &response, nil
}

// OutputObjects is useful for debugging
func (c *FakeDatastore) OutputObjects() {
	fmt.Fprintln(os.Stdout, "------------start")
	for k, v := range c.objects {
		var e datastorepb.Entity
		if err := proto.Unmarshal(v, &e); err != nil {
			fmt.Fprintln(os.Stdout, "unmarshal error for key:", k, " error:", err)
		} else {
			fmt.Fprintln(os.Stdout, "key: ", k, "value: ", e.String())
		}
	}
	fmt.Fprintln(os.Stdout, "------------end")
}

func entityResultFromKey(pbkey *datastorepb.Key) *datastorepb.EntityResult {
	e := &datastorepb.Entity{
		Key: pbkey,
	}
	er := &datastorepb.EntityResult{
		Entity:  e,
		Version: 0, // TODO:Dunno what this is supposed to be
		Cursor:  nil,
	}
	return er
}

func entityResultFromEntity(e *datastorepb.Entity) *datastorepb.EntityResult {
	er := &datastorepb.EntityResult{
		Entity:  e,
		Version: 0, // TODO:Dunno what this is supposed to be
		Cursor:  nil,
	}
	return er
}

// protoKeyToKeyName decodes a protocol buffer representation of a key into an
// equivalent *datastore.Key string.
func protoKeyToKeyName(p *datastorepb.Key) string {
	var namespace string
	if partition := p.PartitionId; partition != nil {
		namespace = partition.NamespaceId
	}
	keyName, kind := getKeyNameAndKindFromPath(p.Path)

	return fmt.Sprintf("%s/%s/%s", namespace, kind, keyName)
}

func getKeyNameAndKindFromPath(path []*datastorepb.Key_PathElement) (name string, kind string) {
	for _, el := range path {
		kind = el.Kind
		name = el.GetName()
		if name != "" && el.GetId() != 0 {
			name = strconv.FormatInt(el.GetId(), 10)
		}
	}
	return name, kind
}

func protoToKey(p *datastorepb.Key) *datastore.Key {
	var key *datastore.Key
	var namespace string
	if partition := p.PartitionId; partition != nil {
		namespace = partition.NamespaceId
	}
	for _, el := range p.Path {
		key = &datastore.Key{
			Namespace: namespace,
			Kind:      el.Kind,
			ID:        el.GetId(),
			Name:      el.GetName(),
			Parent:    key,
		}
	}

	return key
}

// WhyInvalidKey returns why the key is valid. useful for debugging
func WhyInvalidKey(k *datastore.Key) {
	if k == nil {
		fmt.Fprintln(os.Stdout, "\n**key was nil")
	}
	for ; k != nil; k = k.Parent {
		if k.Kind == "" {
			fmt.Fprintln(os.Stdout, "\n**key had empty Kind")
		}
		if k.Name != "" && k.ID != 0 {
			fmt.Fprintln(os.Stdout, "\n**key had empty Name or ID = 0")
		}

		if k.Parent != nil {
			if k.Parent.Incomplete() {
				fmt.Fprintln(os.Stdout, "\n**key had incomplete parent")
			}
			if k.Parent.Namespace != k.Namespace {
				fmt.Fprintln(os.Stdout, "\n**key had different namespace from parent")
			}
		}
	}
}

/* TODO(steve): implement remaining methods as necessary

func (c *FakeDatastore) RunQuery(context.Context, *datastorepb.RunQueryRequest) (*datastorepb.RunQueryResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RunQuery not implemented")
}
func (c *FakeDatastore) BeginTransaction(context.Context, *datastorepb.BeginTransactionRequest) (*datastorepb.BeginTransactionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method BeginTransaction not implemented")
}

func (c *FakeDatastore) Rollback(context.Context, *datastorepb.RollbackRequest) (*datastorepb.RollbackResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Rollback not implemented")
}
func (c *FakeDatastore) AllocateIds(context.Context, *datastorepb.AllocateIdsRequest) (*datastorepb.AllocateIdsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method AllocateIds not implemented")
}
func (c *FakeDatastore) ReserveIds(context.Context, *datastorepb.ReserveIdsRequest) (*datastorepb.ReserveIdsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ReserveIds not implemented")
}

*/
