package dstest

import (
	"encoding/xml"
	"testing"

	"gopkg.in/yaml.v2"

	"github.com/Khan/districts-jobs/pkg/khantest"
)

type indexYamlSuite struct{ khantest.Suite }

func (suite *indexYamlSuite) _parse(xmlData, yamlData string) ([]_index, []_index) {
	var xmlIndexes _indexes
	err := xml.Unmarshal([]byte(xmlData), &xmlIndexes)
	suite.Require().NoError(err)

	var yamlIndexes _indexes
	err = yaml.Unmarshal([]byte(yamlData), &yamlIndexes)
	suite.Require().NoError(err)

	return xmlIndexes.Indexes, yamlIndexes.Indexes
}

// To avoid locking and releasing a datastore emulator a bunch of times,
// this test has a few assertions that test how the flow here is
// supposed to work end to end.
func (suite *indexYamlSuite) TestComparison() {
	xmlData := `
<!-- Indices written at Tue, 9 Mar 2021 11:06:57 PST -->
<datastore-indexes autoGenerate="true">
    <!-- Used 1 time in query history -->
    <datastore-index kind="AccountDeletionRequest" ancestor="false"
                     source="auto">
        <property name="cancelled" direction="asc"/>
        <property name="fulfilled" direction="asc"/>
        <property name="date" direction="desc"/>
    </datastore-index>
    <datastore-index kind="FrozenModelStore" ancestor="true" source="auto">
        <property name="index" direction="asc"/>
    </datastore-index>
</datastore-indexes>
`
	yamlData := `
indexes:
- kind: AccountDeletionRequest
  properties:
  - name: cancelled
  - name: fulfilled
  - name: date
    direction: desc
- ancestor: true
  kind: FrozenModelStore
  properties:
  - name: index
`

	xmlIndexes, yamlIndexes := suite._parse(xmlData, yamlData)

	suite.Require().Equal(2, len(xmlIndexes))
	suite.Require().Equal(2, len(yamlIndexes))
	suite.Require().Equal(xmlIndexes[0].String(), yamlIndexes[0].String())
	suite.Require().Equal(xmlIndexes[1].String(), yamlIndexes[1].String())
}

func (suite *indexYamlSuite) TestSuccessfulSubset() {
	xmlData := `
<datastore-indexes autoGenerate="true">
    <datastore-index kind="FrozenModelStore" ancestor="true" source="auto">
        <property name="index" direction="asc"/>
    </datastore-index>
</datastore-indexes>
`
	yamlData := `
indexes:
- kind: AccountDeletionRequest
  properties:
  - name: cancelled
  - name: fulfilled
  - name: date
    direction: desc
- ancestor: true
  kind: FrozenModelStore
  properties:
  - name: index
`

	xmlIndexes, yamlIndexes := suite._parse(xmlData, yamlData)

	suite.Require().Equal([]_index{}, _setDifference(xmlIndexes, yamlIndexes))
}

func (suite *indexYamlSuite) TestUnsuccessfulSubset() {
	xmlData := `
<datastore-indexes autoGenerate="true">
    <datastore-index kind="AccountDeletionRequest" ancestor="false"
                     source="auto">
        <property name="cancelled" direction="asc"/>
        <property name="fulfilled" direction="asc"/>
        <property name="date" direction="desc"/>
    </datastore-index>
    <datastore-index kind="FrozenModelStore" ancestor="true" source="auto">
        <property name="index" direction="asc"/>
    </datastore-index>
</datastore-indexes>
`
	yamlData := `
indexes:
- kind: AccountDeletionRequest
  properties:
  - name: cancelled
  - name: fulfilled
  - name: date
    direction: desc
- kind: FrozenModelStore
  properties:
  - name: index
`

	xmlIndexes, yamlIndexes := suite._parse(xmlData, yamlData)

	// FrozenModelStore differs: the yaml version has ancestor==false.
	suite.Require().Equal([]_index{xmlIndexes[1]}, _setDifference(xmlIndexes, yamlIndexes))
}

func TestIndexYaml(t *testing.T) {
	khantest.Run(t, new(indexYamlSuite))
}
