package pstest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"

	"github.com/Khan/districts-jobs/pkg/errors"
)

var (
	_pubsubData   []_pubsubYamlEntry
	_loadYamlOnce sync.Once
)

// NewTestClient creates a test server and a test-client that connects
// to it.  The test is responsible for calling Close() on both of them
// at the end of the test.
func NewTestClient(ctx context.Context) (*pubsub.Client, *pstest.Server, error) {
	// This is taken from the example at
	// https://godoc.org/cloud.google.com/go/pubsub/pstest#NewServer
	srv := pstest.NewServer()

	//nolint:ka-always-close // added to options
	conn, err := grpc.Dial(
		srv.Addr,
		grpc.WithInsecure(),
	) //nolint:staticcheck // deprecated but ok for now
	if err != nil {
		srv.Close()
		return nil, nil, errors.Wrap(err, "unable to create grpc dialer")
	}

	options := []option.ClientOption{
		option.WithGRPCConn(conn),
	}
	client, err := NewClient(ctx, "khan-test", options)
	if err != nil {
		srv.Close()
		return nil, nil, errors.Wrap(err, "unable to get pubsub client")
	}

	// Unlike for the dev client, we don't protect this with a
	// Once because we need to re-create our pubsub state for each
	// test.
	_autoRegisterPubsubYaml(ctx, client, nil)
	// Don't let the auto-registration we just did pollute the
	// message-space of our tests
	srv.ClearMessages()

	return client, srv, nil
}

// NewClient returns a pubsub Client given options.
//
// Application code will call one of the new-client functions below,
// which set up the options for you.
//
// If projectID is empty, we fall back to the GOOGLE_CLOUD_PROJECT environment
// variable as a default.
func NewClient(
	ctx context.Context,
	projectID string,
	options []option.ClientOption,
) (*pubsub.Client, error) {
	if projectID == "" {
		projectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
		if projectID == "" {
			return nil, errors.Internal(
				"Cannot connect to pubsub: $GOOGLE_CLOUD_PROJECT not set")
		}
	}
	c, err := pubsub.NewClient(ctx, projectID, options...)
	if err != nil {
		return nil, errors.Internal("Error creating pubsub client", err)
	}

	return c, nil
}

func CommandWithBasePath(command string, out io.Writer, basePath string, cmds []string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command(fmt.Sprintf("%s.exe", command), cmds...)
	case "linux", "darwin":
		cmd = exec.Command(command, cmds...)
	default:
		return errors.New("unsupported platform")
	}
	cmd.Dir = basePath
	// for verbose output
	// log.Println(command, cmds)

	cmd.Stdin = os.Stdin
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}

	return cmd.Run()
}

func GitRepoLocalRoot(basepath string) (string, error) {
	var buf bytes.Buffer
	err := gitCommandWithBasePath(&buf, basepath, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return "", errors.WrapWithFields(err, errors.Fields{"git-rev-parse-output": buf.String()})
	}
	return strings.TrimSpace(buf.String()), nil
}

// TODO(csilvers): override all methods of gPubsub.Client that return
// an error, to return a wrapped error instead?  This is useful, but
// makes Client inconsistent with all the other pubsub classes which
// is kinda weird.
type _pubsubYamlEntry struct {
	Subscriptions map[string]struct {
		Endpoint            string `yaml:"endpoint"`
		RetainAckedMessages bool   `yaml:"retainAckedMessages"`
		AckDeadlineSeconds  int    `yaml:"ackDeadlineSeconds"`
	} `yaml:"subscriptions"`
	Topic string `yaml:"topic"`
}

func getWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PWD") // not as reliable, but can't error!
	}
	return cwd
}

// Automatically register all the topics and subscriptions in
// pubsub.yaml, just like we do at deploy-time for prod.  Used for dev
// and tests.
func _autoRegisterPubsubYaml(ctx context.Context, client *pubsub.Client, httpClient *http.Client) {
	_loadYamlOnce.Do(func() {
		err := _loadPubsubYaml(ctx)
		if err != nil {
			panic("Error loading pubsub.yaml: " + err.Error())
		}
	})

	for _, topicInfo := range _pubsubData {
		// Create the topic in pubsub-emulator.  A noop if it already exists.
		_, _ = client.CreateTopic(ctx, topicInfo.Topic)
		topic := client.Topic(topicInfo.Topic)
		for subname, options := range topicInfo.Subscriptions {
			subConfig := pubsub.SubscriptionConfig{Topic: topic}
			// if we were running an emulator... we would need these:
			//  if options.Endpoint != "" {
			//  	subConfig.PushConfig = pubsub.PushConfig{
			//  		Endpoint: _endpointToDev(ctx, options.Endpoint, httpClient),
			//  	}
			//}
			if options.RetainAckedMessages {
				subConfig.RetainAckedMessages = options.RetainAckedMessages
			}
			if options.AckDeadlineSeconds != 0 {
				subConfig.AckDeadline = time.Duration(options.AckDeadlineSeconds) * time.Second
			}
			_, _ = client.CreateSubscription(ctx, subname, subConfig)
		}
	}
}

func _loadPubsubYaml(ctx context.Context) error {
	var err error

	wd := getWD()
	repoRoot, err := GitRepoLocalRoot(wd)
	if err != nil {
		panic(err)
	}
	filename := filepath.Join(repoRoot, "pkg/gcpapi/pubsub/pstest/pubsub.yaml")

	file, err := os.Open(filename)
	if err != nil {
		return errors.Wrap(err, "unable to open file: "+filename)
	}
	defer file.Close()

	yamlData, err := io.ReadAll(file)
	if err != nil {
		return errors.Wrap(err, "unable to read file: "+filename)
	}

	return errors.Wrap(yaml.Unmarshal(yamlData, &_pubsubData), "unable to unmarshal pubsub.yaml")
}

func gitCommandWithBasePath(out io.Writer, basePath string, cmds []string) error {
	return CommandWithBasePath("git", out, basePath, cmds)
}
