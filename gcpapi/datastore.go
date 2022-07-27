package gcpapi

import (
	"context"

	"cloud.google.com/go/datastore"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	"github.com/Khan/districts-jobs/pkg/errors"
)

func NewDataStoreClient(
	ctx context.Context,
	credentials []byte,
) (client *datastore.Client, err error) {
	if len(credentials) != 0 {
		return datastore.NewClient(
			ctx,
			"khan-academy",
			option.WithCredentialsJSON(credentials),
		)
	}
	return datastore.NewClient(ctx, "khan-academy")
}

// This communicates to the grpc-translator running in the python monolith.
// Unless that is not running. Then it just dies.
func NewDevDatastoreClient(ctx context.Context) (*datastore.Client, error) {
	return newDevClient(ctx)
}

// NewDevClient sets up a client talking to the persistent datastore emulator.
//
// We will connect to the Python (devappserver) datastore via the
// grpc-translator, rather than the standalone datastore emulator.
func newDevClient(ctx context.Context) (*datastore.Client, error) {
	// datastoreService := "grpc-translator"
	// Since we're talking to the Python datastore (via the grpc-translator),
	// we need our datastore project to match Python's.  (Note that
	// devappserver/datastore-translator automatically add a dev~ prefix.)
	// TODO(benkraft): Do this in a more principled way, perhaps by
	// changing the devappserver project ID to khan-dev, rather than
	// just overriding GOOGLE_CLOUD_PROJECT with a hardcoded value
	projectID := "khan-academy"

	// This will automatically connect to a running emulator
	// running unit tests, which may lead to tests failing on
	// jenkins or when test data is not cleaned up.  Devs should
	// use TempClient instead to avoid these errors, generally by
	// using `servicetest.Suite` as the base of their tests.
	emulatorHost := "localhost:8205"

	return NewClientWithInsecureEndpoint(ctx, emulatorHost, projectID)
}

// NewClientWithInsecureEndpoint sets up a client with a given endpoint,
// connecting insecurely.
//
// Application code should typically call e.g. NewDevClient instead which
// computes the endpoint automatically; this is exported for the benefit of the
// test client.
func NewClientWithInsecureEndpoint(
	ctx context.Context,
	endpoint, projectID string,
) (*datastore.Client, error) {
	options := []option.ClientOption{
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithInsecure()),
	}

	return newClient(ctx, options, projectID)
}

func newClient(
	ctx context.Context,
	options []option.ClientOption,
	projectID string,
) (*datastore.Client, error) {
	if projectID == "" {
		return nil, errors.Newf(
			"cannot connect to the datastore: $GOOGLE_CLOUD_PROJECT not set")
	}

	client, err := datastore.NewClient(ctx, projectID, options...)
	if err != nil {
		return nil, errors.Newf("error creating datastore client %+v", err)
	}

	return client, nil
}
