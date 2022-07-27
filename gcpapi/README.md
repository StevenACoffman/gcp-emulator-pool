### GCPAPI

This directory is for the boilerplate associated with using various GCP services.

Mostly, this is what you need to do to create a client for CloudStore, Dataflow, DataStore, including
reading the credentials file for a service account.

### On Testing GCP clients and APIs

In addition to convenience methods for working with GCP, this package facilitates testing those interactions.

1. The best way to write unit tests with GCP clients is to use a fake server. The in-memory fakes for [bigtable](https://github.com/GoogleCloudPlatform/google-cloud-go/blob/master/bigtable/bttest) and [logging](https://github.com/GoogleCloudPlatform/google-cloud-go/tree/master/logging/internal/testing) are the best examples. It's easy to set up an in-memory gRPC server, and we even have some [helpers](./testutil/server.go) for that, which you can use. But building and maintaining a fake for a complex API like BigTable or Spanner is expensive. The Cloud Bigtable team supports their fake, but the Spanner team does not intend to provide a fake, and some Google service teams do not have the bandwidth to write one.
2. The next best approach is a mock server. You'd use the same in-memory server technology, but provide the server with expected request-response pairs instead of having it simulate the service. I don't think there are any full-fledged mocks at present, although Google do use limited mocking throughout for some tests (as in these [pubsub](https://github.com/GoogleCloudPlatform/google-cloud-go/blob/master/pubsub/puller_test.go) and [storage](https://github.com/GoogleCloudPlatform/google-cloud-go/blob/master/storage/writer_test.go) tests).
3. A third option is to use RPC replay. The idea is that you write an integration test, then run it against the real service and capture the server traffic. Future calls to the integration test (with an appropriate flag) run against the stored server responses. This is essentially an automated mock.

* [Lightweight in-memory PubSub fake](https://godoc.org/cloud.google.com/go/pubsub/pstest)
* [gRPC replay tool (for clients other than storage and bigquery)](https://godoc.org/cloud.google.com/go/rpcreplay)
* [HTTP replay tool (for storage and bigquery)](https://godoc.org/cloud.google.com/go/httpreplay)
* [Interface packages for mocking Storage and BigQuery](https://github.com/GoogleCloudPlatform/google-cloud-go-testing)
* [Google Cloud Testing Guide](https://github.com/googleapis/google-cloud-go/blob/master/testing.md) - TLDR; Can fake /w grpc.Server. Or define your own interface for methods you use and mock it.



