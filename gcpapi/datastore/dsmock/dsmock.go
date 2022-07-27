// Package dsmock implements a fake dsiface.Client
// If you make changes to existing code, please test whether it breaks
// existing clients, e.g. in etl-gardener.
package dsmock

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"reflect"
	"sync"

	"cloud.google.com/go/datastore" //nolint:depguard // GKE ≠ AppEngine
	"github.com/googleapis/google-cloud-go-testing/datastore/dsiface"

	"github.com/Khan/districts-jobs/pkg/errors"
)

// NOTE: This is over-restrictive, but fine for current purposes.
func validateDatastoreEntity(e interface{}) error {
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Ptr {
		return datastore.ErrInvalidEntityType
	}
	// NOTE: This is over-restrictive, but fine for current purposes.
	if reflect.Indirect(v).Kind() != reflect.Struct {
		return datastore.ErrInvalidEntityType
	}
	return nil
}

// ErrNotImplemented is returned if a dsiface function is unimplemented.
var ErrNotImplemented = errors.New("not implemented")

// Client implements a crude datastore test client.  It is somewhat
// simplistic and incomplete.  It works only for basic Put, Get, and Delete,
// but may not always work correctly.
type Client struct {
	dsiface.Client // For unimplemented methods
	lock           sync.Mutex
	objects        map[datastore.Key][]byte
}

// NewClient returns a fake client that satisfies dsiface.Client.
func NewClient() *Client {
	if flag.Lookup("test.v") == nil {
		log.Fatal("DSFakeClient should only be used in tests")
	}
	return &Client{objects: make(map[datastore.Key][]byte, 10)}
}

// Close implements dsiface.Client.Close
func (c *Client) Close() error { return nil }

// Count implements dsiface.Client.Count
func (c *Client) Count(ctx context.Context, q *datastore.Query) (n int, err error) {
	return 0, ErrNotImplemented
}

// Delete implements dsiface.Client.Delete
func (c *Client) Delete(ctx context.Context, key *datastore.Key) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	_, ok := c.objects[*key]
	if !ok {
		return datastore.ErrNoSuchEntity
	}
	delete(c.objects, *key)
	return nil
}

// Get implements dsiface.Client.Get
func (c *Client) Get(ctx context.Context, key *datastore.Key, dst interface{}) (err error) {
	err = validateDatastoreEntity(dst)
	if err != nil {
		return err
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	o, ok := c.objects[*key]
	if !ok {
		return datastore.ErrNoSuchEntity
	}
	return json.Unmarshal(o, dst)
}

type multiArgType int

const (
	multiArgTypeInvalid multiArgType = iota
	multiArgTypePropertyLoadSaver
	multiArgTypeStruct
	multiArgTypeStructPtr
	multiArgTypeInterface
)

var (
	typeOfPropertyLoadSaver = reflect.TypeOf((*datastore.PropertyLoadSaver)(nil)).Elem()
	typeOfPropertyList      = reflect.TypeOf(datastore.PropertyList(nil))
)

// checkMultiArg checks that v has type []S, []*S, []I, or []P, for some struct
// type S, for some interface type I, or some non-interface non-pointer type P
// such that P or *P implements PropertyLoadSaver.
//
// It returns what category the slice's elements are, and the reflect.Type
// that represents S, I or P.
//
// As a special case, PropertyList is an invalid type for v.
func checkMultiArg(v reflect.Value) (m multiArgType, elemType reflect.Type) {
	// TODO(djd): multiArg is very confusing. Fold this logic into the
	// relevant Put/Get methods to make the logic less opaque.
	if v.Kind() != reflect.Slice {
		return multiArgTypeInvalid, nil
	}
	if v.Type() == typeOfPropertyList {
		return multiArgTypeInvalid, nil
	}
	elemType = v.Type().Elem()
	if reflect.PtrTo(elemType).Implements(typeOfPropertyLoadSaver) {
		return multiArgTypePropertyLoadSaver, elemType
	}
	switch elemType.Kind() {
	case reflect.Struct:
		return multiArgTypeStruct, elemType
	case reflect.Interface:
		return multiArgTypeInterface, elemType
	case reflect.Ptr:
		elemType = elemType.Elem()
		if elemType.Kind() == reflect.Struct {
			return multiArgTypeStructPtr, elemType
		}
	}
	return multiArgTypeInvalid, nil
}

// valid returns whether the key is valid.
func valid(k *datastore.Key) bool {
	if k == nil {
		return false
	}
	for ; k != nil; k = k.Parent {
		if k.Kind == "" {
			return false
		}
		if k.Name != "" && k.ID != 0 {
			return false
		}
		if k.Parent != nil {
			if k.Parent.Incomplete() {
				return false
			}
			if k.Parent.Namespace != k.Namespace {
				return false
			}
		}
	}
	return true
}

// GetMulti is a batch version of Get.
//
// dst must be a []S, []*S, []I or []P, for some struct type S, some interface
// type I, or some non-interface non-pointer type P such that P or *P
// implements PropertyLoadSaver. If an []I, each element must be a valid dst
// for Get: it must be a struct pointer or implement PropertyLoadSaver.
//
// As a special case, PropertyList is an invalid type for dst, even though a
// PropertyList is a slice of structs. It is treated as invalid to avoid being
// mistakenly passed when []PropertyList was intended.
//
// err may be a MultiError. See ExampleMultiError to check it.
func (c *Client) GetMulti(ctx context.Context, keys []*datastore.Key, dst interface{}) (err error) {
	fmt.Printf("%+v\n", c.objects)
	v := reflect.ValueOf(dst)
	multiArgType, _ := checkMultiArg(v)

	// Sanity checks
	if multiArgType == multiArgTypeInvalid {
		return errors.New("datastore: dst has invalid type")
	}
	if len(keys) != v.Len() {
		return errors.New("datastore: keys and dst slices have different length")
	}
	if len(keys) == 0 {
		return nil
	}

	// Go through keys and validate them,
	multiErr, any := make(datastore.MultiError, len(keys)), false
	for i, k := range keys {
		if !valid(k) {
			multiErr[i] = datastore.ErrInvalidKey
			any = true
		} else if k.Incomplete() {
			multiErr[i] = errors.Newf("datastore: can't get the incomplete key: %v", k)
			any = true
		}
	}
	if any {
		return multiErr
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	for index := range keys {
		value, ok := c.objects[*keys[index]]
		if ok {
			elem := v.Index(index)
			if multiArgType == multiArgTypePropertyLoadSaver ||
				multiArgType == multiArgTypeStruct {
				elem = elem.Addr()
			}
			if multiArgType == multiArgTypeStructPtr && elem.IsNil() {
				elem.Set(reflect.New(elem.Type().Elem()))
			}
			if jsonErr := json.Unmarshal(value, elem.Interface()); jsonErr != nil {
				multiErr[index] = jsonErr
				any = true
			}
		} else {
			multiErr[index] = datastore.ErrNoSuchEntity
			any = true
		}
	}

	if any {
		return multiErr
	}
	return nil
}

// Put implements dsiface.Client.Put
func (c *Client) Put(
	_ context.Context,
	key *datastore.Key,
	src interface{},
) (*datastore.Key, error) {
	err := validateDatastoreEntity(src)
	if err != nil {
		return nil, err
	}
	js, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.objects[*key] = js
	return key, nil
}

// GetKeys lists all keys saved in the fake client.
func (c *Client) GetKeys() []datastore.Key {
	c.lock.Lock()
	defer c.lock.Unlock()
	keys := make([]datastore.Key, len(c.objects))
	i := 0
	for k := range c.objects {
		keys[i] = k
		i++
	}

	return keys
}

func (c *Client) GetMap() map[datastore.Key][]byte {
	c.lock.Lock()
	defer c.lock.Unlock()
	newMap := make(map[datastore.Key][]byte, 10)
	for k, v := range c.objects {
		newMap[k] = v
	}
	return c.objects
}
