package dstest

// This file is responsible for comparing the datastore indexes used
// by a datastore test to the datastore indexes provided by
// index.yaml, and to complain if a test uses an index that's not
// index.yaml.
//
// This can help avoid an error in production where we add a datastore
// query that requires a composite index, and even write a test for
// that query, but then fail in production because the necessary
// composite index was not added to index.yaml.  The datastore
// emulator notices what composite indexes are needed, but doesn't do
// any alerting around it, so we have to do that ourselves.
//
// How the datastore emulator treats composite indexes depends on how
// it's run, but when it's run in `--no-store-on-disk` mode, like we do,
// the datastore emulator just stores all composite queries it uses in
//    <emulator-datadir>/WEB-INF/appengine-generated/datastore-indexes-auto.xml
// as an xml file that looks like this:
//     <!-- Indices written at Tue, 9 Mar 2021 11:06:57 PST -->
//     <datastore-indexes autoGenerate="true">
//         <!-- Used 1 time in query history -->
//         <datastore-index kind="AccountDeletionRequest" ancestor="false"
//                          source="auto">
//             <property name="cancelled" direction="asc"/>
//             <property name="fulfilled" direction="asc"/>
//             <property name="date" direction="desc"/>
//         </datastore-index>
//     </datastore-indexes>
//
// We need to parse that xml, and also parse webapp's index.yaml, and
// compare them.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"

	"github.com/Khan/districts-jobs/pkg/errors"
)

// Both the xml and yaml have the same shape, just different data types!
type _index struct {
	Kind     string `xml:"kind,attr"     yaml:"kind"`
	Ancestor string `xml:"ancestor,attr" yaml:"ancestor"`
	Property []struct {
		Name      string `xml:"name,attr" yaml:"name"`
		Direction string `xml:"direction,attr" yaml:"direction"`
	} `xml:"property"      yaml:"properties"`
}

type _indexes struct {
	Indexes []_index `xml:"datastore-index" yaml:"indexes"`
}

var (
	_yamlIndexes  []_index
	_loadYamlOnce sync.Once
)

// Marshal `_index` into a canonical format.  The particular values
// for some of the booleans difer between xml and yaml, so we normalize.
func (idx _index) String() string {
	retval := idx.Kind
	if idx.Ancestor == "yes" || idx.Ancestor == "true" {
		retval += "[ancestor]"
	}

	properties := make([]string, len(idx.Property))
	for i, property := range idx.Property {
		properties[i] = property.Name
		if property.Direction == "desc" {
			properties[i] += "[desc]"
		}
	}
	sort.Strings(properties)

	retval += "{"
	retval += strings.Join(properties, ",")
	retval += "}"
	return retval
}

// unmarshaler should be xml.Unmarshal, yaml.Unmarshal, etc.
func _readIndex(
	abspath string,
	unmarshaler func([]byte, interface{}) error,
) ([]_index, error) {
	file, err := os.Open(abspath)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var indexes _indexes
	err = unmarshaler(data, &indexes)
	if err != nil {
		return nil, err
	}
	return indexes.Indexes, nil
}

// Return all the `_index` entries in xmlIndexes that are not in
// yamlIndexes.
func _setDifference(xmlIndexes, yamlIndexes []_index) []_index {
	yamlIndexStrings := make(map[string]bool, len(yamlIndexes))
	for _, yamlIndex := range yamlIndexes {
		yamlIndexStrings[yamlIndex.String()] = true
	}

	retval := []_index{}
	for _, xmlIndex := range xmlIndexes {
		if !yamlIndexStrings[xmlIndex.String()] {
			retval = append(retval, xmlIndex)
		}
	}
	return retval
}

// Do our best to make sure there's no index.xml file left over from
// an old test.
func clearIndexXMLFile(emulatorDatadir string) {
	dirname := path.Join(emulatorDatadir, "WEB-INF/appengine-generated")
	filename := path.Join(dirname, "datastore-indexes-auto.xml")
	_ = os.MkdirAll(dirname, 0o755)
	emptyFile, err := os.Create(filename)
	if err == nil {
		_, _ = fmt.Fprintf(emptyFile, "<datastore-indexes />")
		emptyFile.Close()
	}
}

// loadIndexYAML parses index.yaml and stores it in memory in pkg variable.
// It is used for every test but never changes between test runs.
// This is meant to be called when creating the datastore test dsClient.
func loadIndexYAML(ctx context.Context) {
	_loadYamlOnce.Do(func() {
		var err error

		wd := getWD()
		repoRoot, err := GitRepoLocalRoot(wd)
		if err != nil {
			panic(err)
		}
		abspath := filepath.Join(repoRoot, "pkg/gcpapi/datastore/dstest/index.yaml")

		_yamlIndexes, err = _readIndex(abspath, yaml.Unmarshal)
		if err != nil {
			panic("Error loading index.yaml: " + err.Error())
		}
	})
}

// compositeIndexes returns the composite indexes used within the recent test.
func compositeIndexes(emulatorDatadir string) ([]_index, error) {
	abspath := path.Join(
		emulatorDatadir, "WEB-INF/appengine-generated/datastore-indexes-auto.xml")
	return _readIndex(abspath, xml.Unmarshal)
}

// MissingCompositeIndexes returns a human-readable string listing all
// the composite indexes used by the most recent test run in
// emulatorDatadir, that are not also in index.yaml.  It should be
// called at the end of a test, right before the test releases the
// emulator lock.  The return value is the empty string if no indexes
// are missing.
//
// IMPORTANT NOTE: this may give false positives if there are two
// (or more) possible "pefect" indexes for a query, and we have
// one in our index.yaml and the datastore-emulator chooses the
// other.  In such situations, the easiest thing to do is to just
// change ours so it matches the datastore-emulator.  If this is
// not feasible (because we're using the same index for two different
// queries) you may have to special-case that here.
func missingCompositeIndexes(emulatorDatadir string) (string, error) {
	xmlIndexes, err := compositeIndexes(emulatorDatadir)
	if err != nil {
		return "", errors.Internal(
			"Error reading datastore indexes used by test",
			err, errors.Fields{"datadir": emulatorDatadir})
	}
	if len(xmlIndexes) == 0 {
		return "", nil // short-circuit in a common case.
	}

	// The yaml indexes were loaded when the test-dsClient was created,
	// in NewTempClient.

	missingIndexes := _setDifference(xmlIndexes, _yamlIndexes)
	missingIndexStrings := make([]string, len(missingIndexes))
	for i, index := range missingIndexes {
		missingIndexStrings[i] = index.String()
	}
	return strings.Join(missingIndexStrings, "\n"), nil
}
