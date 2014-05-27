// Copyright 2013 Alex Browne.  All rights reserved.
// Use of this source code is governed by the MIT
// license, which can be found in the LICENSE file.

// File transaction.go contains all code dealing with the
// transactions abstraction, including construction, adding
// commands, and execution of the transaction.

package zoom

import (
	"errors"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"reflect"
)

type transaction struct {
	conn       redis.Conn
	commands   []command
	handlers   []func(interface{}) error
	dataReady  map[string]bool
	data       map[string]interface{}
	waiters    []waiter
	modelCache map[string]interface{}
}

type command struct {
	name string
	args []interface{}
}

type waiter struct {
	trans *transaction
	keys  []string
	do    func() error
	done  bool
}

func (w waiter) ready() bool {
	for _, key := range w.keys {
		if key == "" {
			return true
		}
		if !w.trans.dataReady[key] {
			return false
		}
	}
	return true
}

func newTransaction() *transaction {
	t := &transaction{
		conn:       GetConn(),
		modelCache: make(map[string]interface{}),
		dataReady:  make(map[string]bool),
		data:       make(map[string]interface{}),
	}
	return t
}

func (t *transaction) sendData(key string, data interface{}) {
	t.data[key] = data
	t.dataReady[key] = true
}

func (t *transaction) doWhenDataReady(dataKeys []string, do func() error) {
	w := waiter{
		trans: t,
		keys:  dataKeys,
		do:    do,
		done:  false,
	}
	t.waiters = append(t.waiters, w)
}

func (t *transaction) command(cmd string, args []interface{}, handler func(interface{}) error) {
	t.commands = append(t.commands, command{name: cmd, args: args})
	t.handlers = append(t.handlers, handler)
}

func (t *transaction) exec() error {
	defer t.discard()

	// execute any of the waiting functions if they are ready before any commands
	// are run
	if err := t.executeWaitersIfReady(); err != nil {
		return err
	}

	for len(t.commands) > 0 {
		// send all the pending commands at once using a redis transaction
		t.conn.Send("MULTI")
		for _, c := range t.commands {
			if err := t.conn.Send(c.name, c.args...); err != nil {
				return err
			}
		}

		// invoke redis driver to execute the transaction
		replies, err := redis.MultiBulk(t.conn.Do("EXEC"))
		if err != nil {
			t.discard()
			return err
		}

		// iterate through the replies, calling the corresponding handler functions
		for i, reply := range replies {
			handler := t.handlers[i]
			if handler != nil {
				if err := handler(reply); err != nil {
					return err
				}
			}
		}

		// reset all handlers and commands and prepare for the next stage
		t.commands = make([]command, 0)
		t.handlers = make([]func(interface{}) error, 0)

		// execute any of the waiting functions if they are now ready
		if err := t.executeWaitersIfReady(); err != nil {
			return err
		}
	}

	if len(t.waiters) > 0 {
		pendingData := []string{}
		for _, w := range t.waiters {
			for _, key := range w.keys {
				if !t.dataReady[key] {
					pendingData = append(pendingData, key)
				}
			}
		}
		return fmt.Errorf(`zoom: transaction finished executing but some pending data was never sent.
		This is probably either because you forgot to send the data or there was a dependency cycle.
		%d function(s) were still waiting on data to be sent and were never executed.
		The following data was still pending: %v`, len(t.waiters), pendingData)
	}

	return nil
}

func (t *transaction) executeWaitersIfReady() error {
	stillWaiting := make([]waiter, 0)
	for _, w := range t.waiters {
		if w.ready() && !w.done {
			if err := w.do(); err != nil {
				return err
			}
		} else {
			stillWaiting = append(stillWaiting, w)
		}
	}
	t.waiters = stillWaiting

	return nil
}

func (t *transaction) discard() error {
	defer t.conn.Close()
	_, err := t.conn.Do("DISCARD")
	if err != nil {
		return err
	}
	return nil
}

// Useful Handlers

// newScanHandler invokes redis driver to scan single values into the corresponding scannable
func newScanHandler(mr modelRef, scannables []interface{}) func(interface{}) error {
	return func(reply interface{}) error {
		replies, err := redis.Values(reply, nil)
		if err != nil {
			return err
		}
		if len(replies) == 0 {
			// there was a miss
			mr.possibleKeyHits -= 1
			if mr.possibleKeyHits == 0 {
				// if we are out of possible hits (i.e. none of the keys which could have
				// possibly been storing data for the model returned anything), it means
				// one of two things: (1) the model is not in the database or (2) all of
				// the fields/attributes of the model are nil. In case (2) we should
				// simply return an empty model struct. But for case (1) we should return
				// a KeyNotFoundError. We need to do one final thing to differentiate between
				// (1) and (2): check if the model has been indexed in the <modelName>:index
				// set. If it has, it means (2). If it has not, it means (1).
				return checkModelExists(mr)
			}
		}
		if _, err := redis.Scan(replies, scannables...); err != nil {
			return err
		}
		return nil
	}
}

// newSingleScanHandler scans a single value (such as an int or string) into scannable
func newSingleScanHandler(scannable interface{}) func(interface{}) error {
	return func(reply interface{}) error {
		replies := []interface{}{reply}
		if _, err := redis.Scan(replies, scannable); err != nil {
			return err
		} else {
			return nil
		}
	}
}

// newScanModelHandler invokes redis driver to scan multiple values into scannable (a struct)
func newScanModelHandler(mr modelRef) func(interface{}) error {
	return func(reply interface{}) error {
		bulk, err := redis.MultiBulk(reply, nil)
		if err != nil {
			return err
		}
		if len(bulk) == 0 {
			// there was a miss
			mr.possibleKeyHits -= 1
			if mr.possibleKeyHits == 0 {
				// if we are out of possible hits (i.e. none of the keys which could have
				// possibly been storing data for the model returned anything), it means
				// one of two things: (1) the model is not in the database or (2) all of
				// the fields/attributes of the model are nil. In case (2) we should
				// simply return an empty model struct. But for case (1) we should return
				// a KeyNotFoundError. We need to do one final thing to differentiate between
				// (1) and (2): check if the model has been indexed in the <modelName>:index
				// set. If it has, it means (2). If it has not, it means (1).
				return checkModelExists(mr)
			}
		}
		if err := scanModel(bulk, mr); err != nil {
			return err
		} else {
			return nil
		}
	}
}

// newScanModelSliceHandler invokes redis driver to scan multiple values into a single
// slice or array. The reflect.Value of the slice or array should be passed as an argument.
// it requires a passed in mr and keeps track of miss counts. Will return an error
// if the model does not exist. Use this for scanning into a struct field.
func newScanModelSliceHandler(mr modelRef, scanVal reflect.Value) func(interface{}) error {
	return func(reply interface{}) error {
		bulk, err := redis.MultiBulk(reply, nil)
		if err != nil {
			return err
		}
		if len(bulk) == 0 {
			// there was a miss
			mr.possibleKeyHits -= 1
			if mr.possibleKeyHits == 0 {
				// if we are out of possible hits (i.e. none of the keys which could have
				// possibly been storing data for the model returned anything), it means
				// one of two things: (1) the model is not in the database or (2) all of
				// the fields/attributes of the model are nil. In case (2) we should
				// simply return an empty model struct. But for case (1) we should return
				// a KeyNotFoundError. We need to do one final thing to differentiate between
				// (1) and (2): check if the model has been indexed in the <modelName>:index
				// set. If it has, it means (2). If it has not, it means (1).
				return checkModelExists(mr)
			}
		}
		scanType := scanVal.Type()
		scanElem := scanType.Elem()
		for _, el := range bulk {
			srcElem := reflect.ValueOf(el)
			converted := srcElem.Convert(scanElem)
			scanVal.Set(reflect.Append(scanVal, converted))
		}
		return nil
	}
}

// newScanSliceHandler invokes redis driver to scan multiple values into a single
// slice or array. The reflect.Value of the slice or array should be passed as an argument.
// it defers from newScanModelSliceHandler in that it does not require a modelRef argument
// and does not keep track of misses. Use this internally for anything that is not part
// of a model.
func newScanSliceHandler(scannable interface{}) func(interface{}) error {
	return func(reply interface{}) error {
		bulk, err := redis.MultiBulk(reply, nil)
		if err != nil {
			return err
		}
		scanVal := reflect.ValueOf(scannable).Elem()
		scanType := scanVal.Type()
		scanElem := scanType.Elem()
		for _, el := range bulk {
			srcElem := reflect.ValueOf(el)
			converted := srcElem.Convert(scanElem)
			scanVal.Set(reflect.Append(scanVal, converted))
		}
		return nil
	}
}

// newStringSliceChannelHandler accepts a chan []string as an argument. It
// returns a function which, when executed, will attempt to convert a reply from a
// redis command into a string slice and send it back through the given channel.
func newStringSliceChannelHandler(channel chan []string) func(interface{}) error {
	return func(reply interface{}) error {
		if strings, err := redis.Strings(reply, nil); err != nil {
			return err
		} else {
			channel <- strings
			return nil
		}
	}
}

// newSendDataHandler returns a function which will send the reply of the command
// to the transaction data
func newSendDataHandler(t *transaction, key string) func(interface{}) error {
	return func(reply interface{}) error {
		t.sendData(key, reply)
		return nil
	}
}

// Useful Operations for Transactions

// saveModel adds all the necessary commands to save a given model to the redis database
// this includes indeces and external sets/lists
func (t *transaction) saveModel(m Model) error {
	mr, err := newModelRefFromModel(m)
	if err != nil {
		return err
	}

	// set the id if needed
	if m.GetId() == "" {
		m.SetId(generateRandomId())
	}

	// add operations to save the model indexes
	// we do this first becuase it may require a read before write :(
	if err := t.saveModelIndexes(mr); err != nil {
		return err
	}

	// add an operation to write data to database
	if err := t.saveStruct(mr); err != nil {
		return err
	}

	// add an operation to add to index for this model
	t.index(mr)

	// add operations to save external lists and sets
	t.saveModelLists(mr)
	t.saveModelSets(mr)

	// add operations to save model relationships
	t.saveModelRelationships(mr)
	return nil
}

func (t *transaction) saveStruct(mr modelRef) error {
	if args, err := mr.mainHashArgs(); err != nil {
		return err
	} else {
		if len(args) > 1 {
			t.command("HMSET", args, nil)
		}
		return nil
	}
}

func (t *transaction) index(mr modelRef) {
	args := redis.Args{}.Add(mr.indexKey()).Add(mr.model.GetId())
	t.command("SADD", args, nil)
}

func (t *transaction) saveModelLists(mr modelRef) {
	for _, list := range mr.modelSpec.lists {
		field := mr.value(list.fieldName)
		if field.IsNil() {
			continue // skip empty lists
		}
		listKey := mr.key() + ":" + list.redisName
		args := redis.Args{}.Add(listKey).AddFlat(field.Interface())
		t.command("RPUSH", args, nil)
	}
}

func (t *transaction) saveModelSets(mr modelRef) {
	for _, set := range mr.modelSpec.sets {
		field := mr.value(set.fieldName)
		if field.IsNil() {
			continue // skip empty sets
		}
		setKey := mr.key() + ":" + set.redisName
		args := redis.Args{}.Add(setKey).AddFlat(field.Interface())
		t.command("SADD", args, nil)
	}
}

func (t *transaction) saveModelRelationships(mr modelRef) error {
	for _, r := range mr.modelSpec.relationships {
		if r.rType == oneToOne {
			if err := t.saveModelOneToOneRelationship(mr, r); err != nil {
				return err
			}
		} else if r.rType == oneToMany {
			if err := t.saveModelOneToManyRelationship(mr, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *transaction) saveModelOneToOneRelationship(mr modelRef, r relationship) error {
	field := mr.value(r.fieldName)
	if field.IsNil() {
		return nil
	}
	rModel, ok := field.Interface().(Model)
	if !ok {
		msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", field.Type().String())
		return errors.New(msg)
	}
	if rModel.GetId() == "" {
		msg := fmt.Sprintf("zoom: cannot save a relation for a model with no Id: %+v\n. Must save the related model first.", rModel)
		return errors.New(msg)
	}

	// add a command to the transaction to set the relation key
	relationKey := mr.key() + ":" + r.redisName
	args := redis.Args{relationKey, rModel.GetId()}
	t.command("SET", args, nil)
	return nil
}

func (t *transaction) saveModelOneToManyRelationship(mr modelRef, r relationship) error {
	field := mr.value(r.fieldName)
	if field.IsNil() {
		return nil
	}

	// get a slice of ids from the elements of the field
	ids := make([]string, 0)
	for i := 0; i < field.Len(); i++ {
		rElem := field.Index(i)

		// convert the individual element to a model
		rModel, ok := rElem.Interface().(Model)
		if !ok {
			msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", field.Type().String())
			return errors.New(msg)
		}

		// make sure the id is not nil
		if rModel.GetId() == "" {
			msg := fmt.Sprintf("zoom: cannot save a relation for a model with no Id: %+v\n. Must save the related model first.", rModel)
			return errors.New(msg)
		}

		// add its id to the slice
		ids = append(ids, rModel.GetId())
	}

	if len(ids) > 0 {

		// add a command to the transaction to save the ids
		relationKey := mr.key() + ":" + r.redisName
		args := redis.Args{}.Add(relationKey).AddFlat(ids)
		t.command("SADD", args, nil)
	}
	return nil
}

func (t *transaction) saveModelIndexes(mr modelRef) error {
	for _, p := range mr.modelSpec.primativeIndexes {
		if p.indexType == indexNumeric {
			if err := t.saveModelPrimativeIndexNumeric(mr, p); err != nil {
				return err
			}
		} else if p.indexType == indexAlpha {
			t.saveModelPrimativeIndexAlpha(mr, p)
		} else if p.indexType == indexBoolean {
			t.saveModelPrimativeIndexBoolean(mr, p)
		}
	}

	for _, p := range mr.modelSpec.pointerIndexes {
		if p.indexType == indexNumeric {
			if err := t.saveModelPointerIndexNumeric(mr, p); err != nil {
				return err
			}
		} else if p.indexType == indexAlpha {
			t.saveModelPointerIndexAlpha(mr, p)
		} else if p.indexType == indexBoolean {
			t.saveModelPointerIndexBoolean(mr, p)
		}
	}
	return nil
}

func (t *transaction) saveModelPrimativeIndexNumeric(mr modelRef, p primative) error {
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	score, err := convertNumericToFloat64(mr.value(p.fieldName))
	if err != nil {
		return err
	}
	id := mr.model.GetId()
	t.indexNumeric(indexKey, score, id)
	return nil
}

func (t *transaction) saveModelPointerIndexNumeric(mr modelRef, p pointer) error {
	if mr.value(p.fieldName).IsNil() {
		// TODO: special case for indexing nil pointers?
		return nil // skip nil pointers for now
	}
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	score, err := convertNumericToFloat64(mr.value(p.fieldName).Elem())
	if err != nil {
		return err
	}
	id := mr.model.GetId()
	t.indexNumeric(indexKey, score, id)
	return nil
}

func (t *transaction) indexNumeric(indexKey string, score float64, id string) {
	args := redis.Args{}.Add(indexKey).Add(score).Add(id)
	t.command("ZADD", args, nil)
}

func (t *transaction) saveModelPrimativeIndexAlpha(mr modelRef, p primative) {
	t.removeOldAlphaIndex(mr, p.fieldName, p.redisName)
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	value := mr.value(p.fieldName).String()
	id := mr.model.GetId()
	t.indexAlpha(indexKey, value, id)
}

func (t *transaction) saveModelPointerIndexAlpha(mr modelRef, p pointer) {
	t.removeOldAlphaIndex(mr, p.fieldName, p.redisName)
	if mr.value(p.fieldName).IsNil() {
		// TODO: special case for indexing nil pointers?
		return // skip nil pointers for now
	}
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	value := mr.value(p.fieldName).Elem().String()
	id := mr.model.GetId()
	t.indexAlpha(indexKey, value, id)
}

func (t *transaction) indexAlpha(indexKey, value, id string) {
	member := value + " " + id
	args := redis.Args{}.Add(indexKey).Add(0).Add(member)
	t.command("ZADD", args, nil)
}

// Remove the alpha index that may have existed before an update or resave of the model
// this requires a read before write, which is a performance penalty but unfortunatlely
// is unavoidable because of the hacky way we're indexing alpha fields.
func (t *transaction) removeOldAlphaIndex(mr modelRef, fieldName string, redisName string) {
	key := mr.key()
	args := redis.Args{}.Add(key).Add(redisName)
	t.command("HGET", args, func(reply interface{}) error {
		if reply == nil {
			return nil
		}
		oldFieldValue, err := redis.String(reply, nil)
		if err != nil {
			return err
		}
		if oldFieldValue == "" {
			return nil
		}
		if oldFieldValue != "" && oldFieldValue != mr.value(fieldName).String() {
			// TODO: Is there a way to do this without creating a new connection?
			// At the very least can we consolidate these operations into a single transaction
			// if there are more than one old indexes to be removed?
			conn := GetConn()
			defer conn.Close()
			alphaIndexKey := mr.modelSpec.modelName + ":" + fieldName
			member := oldFieldValue + " " + mr.model.GetId()
			if _, err := conn.Do("ZREM", alphaIndexKey, member); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *transaction) saveModelPrimativeIndexBoolean(mr modelRef, p primative) {
	value := mr.value(p.fieldName).Bool()
	id := mr.model.GetId()
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	var score float64
	if value == true {
		score = 1.0
	} else {
		score = 0.0
	}
	t.indexNumeric(indexKey, score, id)
}

func (t *transaction) saveModelPointerIndexBoolean(mr modelRef, p pointer) {
	if mr.value(p.fieldName).IsNil() {
		// TODO: special case for indexing nil pointers?
		return // skip nil pointers for now
	}
	value := mr.value(p.fieldName).Elem().Bool()
	id := mr.model.GetId()
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	var score float64
	if value == true {
		score = 1.0
	} else {
		score = 0.0
	}
	t.indexNumeric(indexKey, score, id)
}

func (t *transaction) findModel(mr modelRef, includes []string) error {

	// check model cache to prevent infinite recursion or unnecessary queries
	if prior, found := t.modelCache[mr.key()]; found {
		reflect.ValueOf(mr.model).Elem().Set(reflect.ValueOf(prior).Elem())
		return nil
	}
	t.modelCache[mr.key()] = mr.model

	// scan the hash values directly into the struct
	if includes == nil {
		// use HGETALL to get all the fields for the model
		t.command("HGETALL", redis.Args{}.Add(mr.key()), newScanModelHandler(mr))
	} else {
		// get the appropriate scannable fields
		fields := make([]interface{}, 0)
		for _, fieldName := range includes {
			fields = append(fields, mr.value(fieldName).Addr().Interface())
		}

		// use HMGET to get only certain fields for the model
		if len(fields) != 0 {
			args := redis.Args{}.Add(mr.key()).AddFlat(includes)
			t.command("HMGET", args, newScanHandler(mr, fields))
		}
	}

	// find all the external sets and lists for the model
	if len(mr.modelSpec.lists) != 0 {
		t.findModelLists(mr, includes)
	}
	if len(mr.modelSpec.sets) != 0 {
		t.findModelSets(mr, includes)
	}

	// find the relationships for the model
	if len(mr.modelSpec.relationships) != 0 {
		if err := t.findModelRelationships(mr, includes); err != nil {
			return err
		}
	}

	return nil
}

func (t *transaction) findModelLists(mr modelRef, includes []string) {
	for _, list := range mr.modelSpec.lists {
		if includes != nil {
			if !stringSliceContains(list.fieldName, includes) {
				continue // skip field names that are not in includes
			}
		}

		field := mr.value(list.fieldName)

		// use LRANGE to get all the members of the list
		listKey := mr.key() + ":" + list.redisName
		args := redis.Args{listKey, 0, -1}
		t.command("LRANGE", args, newScanModelSliceHandler(mr, field))
	}
}

func (t *transaction) findModelSets(mr modelRef, includes []string) {
	for _, set := range mr.modelSpec.sets {
		if includes != nil {
			if !stringSliceContains(set.fieldName, includes) {
				continue // skip field names that are not in includes
			}
		}

		field := mr.value(set.fieldName)

		// use SMEMBERS to get all the members of the set
		setKey := mr.key() + ":" + set.redisName
		args := redis.Args{setKey}
		t.command("SMEMBERS", args, newScanModelSliceHandler(mr, field))
	}
}

func (t *transaction) findModelRelationships(mr modelRef, includes []string) error {
	for _, r := range mr.modelSpec.relationships {
		if includes != nil {
			if !stringSliceContains(r.fieldName, includes) {
				continue // skip field names that are not in includes
			}
		}
		if r.rType == oneToOne {
			if err := t.findModelOneToOneRelation(mr, r); err != nil {
				return err
			}
		} else if r.rType == oneToMany {
			if err := t.findModelOneToManyRelation(mr, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *transaction) findModelOneToOneRelation(mr modelRef, r relationship) error {

	// TODO: use scripting to retain integrity of the transaction (we want
	// to perform only one round trip per transaction).
	conn := GetConn()
	defer conn.Close()

	// invoke redis driver to get the id
	relationKey := mr.key() + ":" + r.redisName
	id, err := redis.String(conn.Do("GET", relationKey))
	if err != nil {
		return err
	}

	// instantiate the field using reflection
	field := mr.value(r.fieldName)

	// check if a model with key is already cached in this transaction
	rModelName, _ := getRegisteredNameFromType(field.Type())
	rModelKey := rModelName + ":" + id
	if prior, found := t.modelCache[rModelKey]; found {
		// use the same pointer (it's the same object)
		field.Set(reflect.ValueOf(prior))
		return nil
	} else {
		// create a new pointer
		field.Set(reflect.New(field.Type().Elem()))
	}

	// convert field to a model
	rModel, ok := field.Interface().(Model)
	if !ok {
		msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", field.Type().String())
		return errors.New(msg)
	}

	// set id and create modelRef
	rModel.SetId(id)
	rModelRef, err := newModelRefFromModel(rModel)
	if err != nil {
		return err
	}

	// add a find operation to the transaction
	if err := t.findModel(rModelRef, nil); err != nil {
		return err
	}

	return nil
}

func (t *transaction) findModelOneToManyRelation(mr modelRef, r relationship) error {

	// TODO: use scripting to retain integrity of the transaction (we want
	// to perform only one round trip per transaction).
	conn := GetConn()
	defer conn.Close()

	// invoke redis driver to get a set of keys
	relationKey := mr.key() + ":" + r.redisName
	ids, err := redis.Strings(conn.Do("SMEMBERS", relationKey))
	if err != nil {
		return err
	}

	field := mr.value(r.fieldName)
	rType := field.Type().Elem()

	// iterate through the ids and find each model
	for _, id := range ids {

		// check if a model with key is already cached in this transaction
		rModelName, _ := getRegisteredNameFromType(rType)
		rModelKey := rModelName + ":" + id
		if prior, found := t.modelCache[rModelKey]; found {
			// use the same pointer (it's the same object)
			sliceVal := reflect.Append(field, reflect.ValueOf(prior))
			field.Set(sliceVal)
			continue
		}

		rVal := reflect.New(rType.Elem())
		rModel, ok := rVal.Interface().(Model)
		if !ok {
			msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", rType.String())
			return errors.New(msg)
		}

		// set id and create modelRef
		rModel.SetId(id)
		rModelRef, err := newModelRefFromModel(rModel)
		if err != nil {
			return err
		}

		// add a find operation to the transaction
		if err := t.findModel(rModelRef, nil); err != nil {
			return err
		}

		// append to the field slice
		sliceVal := reflect.Append(field, rVal)
		field.Set(sliceVal)
	}

	return nil
}

func (t *transaction) deleteModel(mr modelRef) {
	modelName := mr.modelSpec.modelName
	id := mr.model.GetId()

	// add an operation to delete the model itself
	key := modelName + ":" + id
	t.delete(key)

	// add an operation to remove the model id from the index
	indexKey := modelName + ":all"
	t.unindex(indexKey, id)

	// add an operation to remove all the field indexes for the model
	t.removeModelIndexes(mr)
}

func (t *transaction) deleteModelById(modelName, id string) error {

	ms, found := modelSpecs[modelName]
	if !found {
		return NewModelNameNotRegisteredError(modelName)
	}

	// add an operation to remove all the field indexes for the model
	// we want to do this first because if there is an error or if the model
	// never existed, there is no need to continue
	if len(ms.primativeIndexes) != 0 || len(ms.pointerIndexes) != 0 {
		m, err := FindById(modelName, id)
		if err != nil {
			if _, ok := err.(*KeyNotFoundError); ok {
				// if it was a key not found error, the model we're trying to delete
				// doesn't exist in the first place. so return nil
				return nil
			} else {
				// this means there was an unexpected error
				return err
			}
		}
		mr, err := newModelRefFromModel(m)
		if err != nil {
			return err
		}
		t.removeModelIndexes(mr)
	}

	// add an operation to delete the model itself
	key := modelName + ":" + id
	t.delete(key)

	// add an operation to remove the model id from the index
	indexKey := modelName + ":all"
	t.unindex(indexKey, id)

	return nil
}

func (t *transaction) delete(key string) {
	t.command("DEL", redis.Args{}.Add(key), nil)
}

func (t *transaction) unindex(key, value string) {
	args := redis.Args{}.Add(key).Add(value)
	t.command("SREM", args, nil)
}

func (t *transaction) removeModelIndexes(mr modelRef) {
	for _, p := range mr.modelSpec.primativeIndexes {
		if p.indexType == indexNumeric {
			t.removeModelPrimativeIndexNumeric(mr, p)
		} else if p.indexType == indexAlpha {
			t.removeModelPrimativeIndexAlpha(mr, p)
		} else if p.indexType == indexBoolean {
			t.removeModelPrimativeIndexBoolean(mr, p)
		}
	}

	for _, p := range mr.modelSpec.pointerIndexes {
		if p.indexType == indexNumeric {
			t.removeModelPointerIndexNumeric(mr, p)
		} else if p.indexType == indexAlpha {
			t.removeModelPointerIndexAlpha(mr, p)
		} else if p.indexType == indexBoolean {
			t.removeModelPointerIndexBoolean(mr, p)
		}
	}
}

func (t *transaction) removeModelPrimativeIndexNumeric(mr modelRef, p primative) {
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	id := mr.model.GetId()
	t.unindexNumeric(indexKey, id)
}

func (t *transaction) removeModelPointerIndexNumeric(mr modelRef, p pointer) {
	if mr.value(p.fieldName).IsNil() {
		// TODO: special case for indexing nil pointers?
		return // skip nil pointers for now
	}
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	id := mr.model.GetId()
	t.unindexNumeric(indexKey, id)
}

func (t *transaction) unindexNumeric(indexKey string, id string) {
	args := redis.Args{}.Add(indexKey).Add(id)
	t.command("ZREM", args, nil)
}

func (t *transaction) removeModelPrimativeIndexAlpha(mr modelRef, p primative) {
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	value := mr.value(p.fieldName).String()
	id := mr.model.GetId()
	t.unindexAlpha(indexKey, value, id)
}

func (t *transaction) removeModelPointerIndexAlpha(mr modelRef, p pointer) {
	if mr.value(p.fieldName).IsNil() {
		// TODO: special case for indexing nil pointers?
		return // skip nil pointers for now
	}
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	value := mr.value(p.fieldName).Elem().String()
	id := mr.model.GetId()
	t.unindexAlpha(indexKey, value, id)
}

func (t *transaction) unindexAlpha(indexKey, value, id string) {
	member := value + " " + id
	args := redis.Args{}.Add(indexKey).Add(member)
	t.command("ZREM", args, nil)
}

func (t *transaction) removeModelPrimativeIndexBoolean(mr modelRef, p primative) {
	id := mr.model.GetId()
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	t.unindexNumeric(indexKey, id)
}

func (t *transaction) removeModelPointerIndexBoolean(mr modelRef, p pointer) {
	id := mr.model.GetId()
	indexKey := mr.modelSpec.modelName + ":" + p.redisName
	t.unindexNumeric(indexKey, id)
}

// check to see if the model id exists in the index. If it doesn't,
// return KeyNotFoundError
func checkModelExists(mr modelRef) error {
	conn := GetConn()
	defer conn.Close()

	indexKey := mr.modelSpec.modelName + ":all"
	if exists, err := redis.Bool(conn.Do("SISMEMBER", indexKey, mr.model.GetId())); err != nil {
		return err
	} else if !exists {
		return NewKeyNotFoundError(mr.key(), mr.modelSpec.modelType)
	}
	return nil
}
