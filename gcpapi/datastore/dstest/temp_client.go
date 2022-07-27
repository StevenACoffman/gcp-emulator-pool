// Package dstest provides utilities for testing code using Google Cloud
// Datastore.
//
// Google provides a "datastore emulator", which is a (Java) binary that
// exposes the same API as the production datastore, but may be run locally for
// development and testing.  In tests, we want each test to run with a fresh
// empty datastore, to ensure the tests are reproducible.  Starting up an
// emulator is slow, so we keep a pool of emulators which tests can use.  This
// package is responsible for managing that pool.  (To allow sharing the pool
// across test processes, we manage it using lock-files rather than in memory.)
//
// Tests don't need to understand any of that; they just need the main export,
// NewTempClient, which talks to such an emulator, abstracting all the
// pool-management.
package dstest

import (
	"context"
	"os"

	"cloud.google.com/go/datastore"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	"github.com/Khan/districts-jobs/pkg/errors"
)

// TempDSClient is a dsClient for talking to a temporary datastore
// (generally a datastore emulator used in tests).
type TempDSClient struct {
	emulator *DatastoreEmulator
	dsClient *datastore.Client
}

// A ResettableClient is a datastore dsClient that can additionally be reset.
//
// It's available for interface upgrades in tests, e.g.
//  ctx.Datastore().(ResettableClient).Reset(ctx)
type ResettableClient interface {
	Reset(context.Context) error
	UsedCompositeIndexes() ([]string, error)
}

// NewTempClient returns a new datastore dsClient for tests talking to a
// local datastore emulator. It will lock an already running datastore
// emulator, or start up a new one if none is present.
//
// Most clients should not need to call this directly; just use
// servicetest.Suite and it will be set up as suite.KAContext().Datastore().
func NewTempClient(ctx context.Context) (*TempDSClient, error) {
	projectID := "khan-test"
	// Set in dev/khantest/suite.go:
	os.Setenv("GOOGLE_CLOUD_PROJECT", projectID)

	emulator, err := acquireDatastoreEmulator(ctx, projectID)
	if err != nil {
		return nil, errors.Wrap(err, "Error starting datastore emulator")
	}

	//rec, err := rpcreplay.NewRecorder("service.replay", nil)
	//if err != nil {
	//	return nil, err
	//}
	//defer func() {
	//	if err := rec.Close(); err != nil {
	//		return
	//	}
	//}()
	//conn, err := grpc.Dial(emulator.Addr, rec.DialOptions()...)

	client, err := datastore.NewClient(ctx,
		projectID,
		option.WithEndpoint(emulator.Addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithInsecure()),
	)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to Create Emulator Datastore Client")
	}

	// Make sure index.yaml is loaded, so we can do some sanity-checks
	// around composite indexes.
	loadIndexYAML(ctx) // in index_yaml.go

	return &TempDSClient{emulator, client}, nil
}

// Reset resets the datastore emulator back to empty.
//
// This is automatically called when acquiring a new emulator, but it's
// available for other clients to call too.
//
// Typically, clients will need to access this method via an interface upgrade:
//  ctx.Datastore().(ResettableClient).Reset(ctx)
func (client *TempDSClient) Reset(ctx context.Context) error {
	return client.emulator.Reset(ctx)
}

func (client *TempDSClient) Datastore() *datastore.Client {
	return client.dsClient
}

func (client *TempDSClient) Emulator() *DatastoreEmulator {
	return client.emulator
}

// UsedCompositeIndexes returns the composite indexes used within the test.
// Tests can assert that an appropriate (or any) index was used.
// Use an interface upgrade: ctx.Datastore().(ResettableClient)
// Calling `Reset` isn't necessary; by default reports on the whole test.
func (client TempDSClient) UsedCompositeIndexes() ([]string, error) {
	indexes, err := compositeIndexes(client.emulator.datadir())
	descs := make([]string, len(indexes))
	for i, index := range indexes {
		descs[i] = index.String()
	}
	return descs, err
}

// Close closes the dsClient's connection and releases our lock on the
// emulator so other tests can use it.
func (client TempDSClient) Close() error {
	// First close the wrapped dsClient connection. We don't immediately
	// return an error here because we want to always unlock the
	// emulator even if closing the connection failed.
	clientErr := client.dsClient.Close()

	emulatorErr := client.emulator.Release()
	// prefer the emulatorError, since it's probably more consequential
	if emulatorErr != nil {
		return errors.Service("could not close emulator", emulatorErr)
	}
	if clientErr != nil {
		return errors.Service("could not close emulator-dsClient", clientErr)
	}
	return nil
}
