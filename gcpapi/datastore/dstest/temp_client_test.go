package dstest

import (
	"context"
	"os"
	"testing"

	"cloud.google.com/go/datastore"

	"github.com/Khan/districts-jobs/pkg/khantest"
	"github.com/Khan/districts-jobs/pkg/models"
)

var EntityKind = models.RegisterNonBackupKind("Entity", new(Entity))

// Entity is used just to test reading and writing from the datastore
type Entity struct {
	Foo string
}

type tempClientSuite struct{ khantest.Suite }

type tempClientContext struct {
	context.Context
}

// The functions we are testing below take a datastore.KAContext but
// don't actually call its Datastore() method.  So we can just provide
// a fake one to make the type system happy.
func (tempClientContext) Datastore() *datastore.Client {
	return nil
}

// To avoid locking and releasing a datastore emulator a bunch of times,
// this test has a few assertions that test how the flow here is
// supposed to work end to end.
func (suite *tempClientSuite) TestTempClient() {
	ctx := tempClientContext{context.Background()}

	client, err := NewTempClient(ctx)
	suite.Require().NoError(err)
	defer client.Close()
	suite.Require().NotNil(client)

	// Make sure it's indeed empty
	query := datastore.NewQuery(EntityKind.Value)
	count, err := client.dsClient.Count(ctx, query)
	suite.Require().NoError(err)
	suite.Require().Equal(0, count)

	// Persist a few entities
	entities := []Entity{{"bar"}, {"baz"}, {"qux"}}
	for _, entity := range entities {
		entity := entity // fix scoping issues; we take a pointer below
		key := datastore.IncompleteKey(EntityKind.Value, nil)
		_, err = client.dsClient.Put(ctx, key, &entity)
		suite.Require().NoError(err)
	}
	count, err = client.dsClient.Count(ctx, query)
	suite.Require().NoError(err)
	suite.Require().Equal(len(entities), count)

	var allEntities []Entity
	_, err = client.dsClient.GetAll(ctx, query, &allEntities)
	suite.Require().NoError(err)
	suite.Require().ElementsMatch(allEntities, entities)

	// Now we reset the datastore and see nothing there
	err = client.Reset(ctx)
	suite.Require().NoError(err)
	indexes, err := client.UsedCompositeIndexes()
	suite.Require().NoError(err)
	suite.Require().Empty(indexes)

	count, err = client.dsClient.Count(ctx, query)
	suite.Require().NoError(err)
	suite.Require().Equal(0, count)
}

func TestTempClient(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("Skipping testing in CI environment")
	}
	khantest.Run(t, new(tempClientSuite))
}
