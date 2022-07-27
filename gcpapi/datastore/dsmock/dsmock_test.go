package dsmock

import (
	"log"
	"testing"

	"cloud.google.com/go/datastore" //nolint:depguard // GKE ≠ AppEngine
)

func init() {
	// Always prepend the filename and line number.
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func must(t *testing.T, err error) {
	if err != nil {
		log.Output(2, err.Error())
		t.Fatal(err)
	}
}

type Object struct {
	Value string
}

func TestDSFake(t *testing.T) {
	client := NewClient()

	const kind = "TestDSFake"
	const namespace = "dsfake"
	k1 := datastore.NameKey(kind, "o1", nil)
	k1.Namespace = namespace
	k2 := datastore.NameKey(kind, "o2", nil)
	k2.Namespace = namespace
	// this key does not get put on purpose!
	k3 := datastore.NameKey(kind, "o3", nil)
	k3.Namespace = namespace

	o1 := Object{"o1"}
	_, err := client.Put(nil, k1, &o1)
	must(t, err)

	var o1a Object
	// This should fail because it o1a isn't a pointer
	err = client.Get(nil, k1, o1a)
	if err != datastore.ErrInvalidEntityType {
		t.Error("Should detect non-pointer")
	}

	must(t, client.Get(nil, k1, &o1a))
	if o1a.Value != o1.Value {
		t.Fatal("Failed put/get", o1a, o1)
	}

	// A second object should not interfere with the first.
	o2 := Object{"o2"}
	_, err = client.Put(nil, k2, &o2)
	must(t, err)

	// Check that Get still fetches the correct o1 value
	var o1b Object
	must(t, client.Get(nil, k1, &o1b))
	if o1b.Value != o1.Value {
		t.Fatal("Apparent object collision", o1b, o1)
	}

	o2.Value = "local-o2"
	// Check that changing original object doesn't change the stored value.
	var o2a Object
	must(t, client.Get(nil, k2, &o2a))
	if o2a.Value != "o2" {
		t.Error("Changing local modifies persisted value", o2a.Value, "!=", "o2")
	}

	keys := client.GetKeys()
	if len(keys) != 2 {
		t.Fatal("Should be 2 keys")
	}
	if keys[0] != *k1 && keys[1] != *k1 {
		t.Error("Missing key1", k1, "\n", keys[0], "\n", keys[1])
	}
	if keys[0] != *k2 && keys[1] != *k2 {
		t.Error("Missing key2", *k2, "\n", keys[0], "\n", keys[1])
	}

	// test GetMulti
	var keysToMultiGet []*datastore.Key
	for i := range keys {
		keysToMultiGet = append(keysToMultiGet, &keys[i])
	}
	keysToMultiGet = append(keysToMultiGet, k3)
	objs := make([]Object, len(keysToMultiGet))
	err = client.GetMulti(nil, keysToMultiGet, objs)
	numNotFounds := 0
	if err != nil {
		if mErr, ok := err.(datastore.MultiError); ok {
			for _, sErr := range mErr {
				if sErr != nil && sErr == datastore.ErrNoSuchEntity {
					numNotFounds++
				} else if sErr != nil {
					t.Fatalf("GetMutli failed %+v", sErr)
				}
			}
		} else {
			t.Fatal("GetMulti got some other error")
		}
	}
	if !contains(objs, o1) {
		t.Fatal("o1 not in returned objects")
	}
	o2orig := Object{"o2"}
	if !contains(objs, o2orig) {
		t.Fatal("o2 original not in returned objects")
	}

	if numNotFounds != 1 {
		t.Errorf("Got numNotFounds: %d, want: %d", numNotFounds, 1)
	}

	// Test Delete()
	must(t, client.Delete(nil, k1))
	err = client.Get(nil, k1, &o1b)
	if err != datastore.ErrNoSuchEntity {
		t.Fatal("delete failed")
	}
}

func contains(s []Object, e Object) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
