package gcpapi

import (
	"context"

	"golang.org/x/oauth2/google"
	dataflow "google.golang.org/api/dataflow/v1b3"
	"google.golang.org/api/option"

	"github.com/Khan/districts-jobs/pkg/errors"
)

func NewDataflowService(
	ctx context.Context,
	credentials []byte,
) (*dataflow.Service, error) {
	oauthClient, err := google.DefaultClient(ctx, dataflow.CloudPlatformScope)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to get New Dataflow client")
	}
	var dataflowService *dataflow.Service
	var sErr error
	if len(credentials) > 0 {
		dataflowService, sErr = dataflow.NewService(
			ctx,
			option.WithHTTPClient(oauthClient),
			option.WithCredentialsJSON(credentials),
		)
	} else {
		dataflowService, sErr = dataflow.NewService(
			ctx,
			option.WithHTTPClient(oauthClient),
		)
	}
	return dataflowService, errors.Wrap(sErr, "Unable to get New Dataflow client")
}
