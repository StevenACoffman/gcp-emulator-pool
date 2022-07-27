# dsifake
Google Cloud Platform datastore interface mock.

+ **Stub** - an object that provides predefined answers to method calls.
+ **Mock** - an object on which you set expectations.
+ **Fake** - an object with limited capabilities (for the purposes of testing), e.g. a fake web service.

Test Double is the general term for stubs, mocks and fakes.

### Why do I want this?
If you interact with Google Cloud Datastore (or firebase in datastore mode) in Go, and you have read
(testing guide)[https://github.com/googleapis/google-cloud-go/blob/master/testing.md]

TLDR; You can fake /w grpc.Server. Or define your own interface for methods you use and mock it. This is the latter.

We mock things based on [googleapis/google-cloud-go-testing](https://github.com/googleapis/google-cloud-go-testing) 
which is now archived.
### Where did you (mostly) steal this?

There were already some mock (not fake) implementations for Get, Put, etc., but not GetMulti:
+ (m-lab/go/cloudtest)[https://pkg.go.dev/github.com/m-lab/go/cloudtest/dsfake]
+ (go build/internal/datastore)[https://github.com/golang/build/tree/master/internal/datastore/fake]

### Your unit test is kinda ... gross?

This whole thing *is* for unit testing, so I didn't spend time 
making my unit testing *of the unit testing fixture* glorious. Got a better idea? Pull Requests welcome.