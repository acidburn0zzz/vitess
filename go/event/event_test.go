package event

import (
	"reflect"
	"testing"
	"time"
)

type testInterface1 interface {
	TestFunc1()
}

type testInterface2 interface {
	TestFunc2()
}

type testEvent1 struct {
}

type testEvent2 struct {
	triggered bool
}

func (testEvent1) TestFunc1() {}

func clearListeners() {
	listenersMutex.Lock()
	defer listenersMutex.Unlock()

	listeners = make(map[reflect.Type][]interface{})
	interfaces = make([]reflect.Type, 0)
}

func TestStaticListener(t *testing.T) {
	clearListeners()

	triggered := false
	AddListener(func(testEvent1) { triggered = true })
	AddListener(func(testEvent2) { t.Errorf("wrong listener type triggered") })
	Dispatch(testEvent1{})

	if !triggered {
		t.Errorf("static listener failed to trigger")
	}
}

func TestPointerListener(t *testing.T) {
	clearListeners()

	testEvent := new(testEvent2)
	AddListener(func(ev *testEvent2) { ev.triggered = true })
	AddListener(func(testEvent2) { t.Errorf("non-pointer listener triggered on pointer type") })
	Dispatch(testEvent)

	if !testEvent.triggered {
		t.Errorf("pointer listener failed to trigger")
	}
}

func TestInterfaceListener(t *testing.T) {
	clearListeners()

	triggered := false
	AddListener(func(testInterface1) { triggered = true })
	AddListener(func(testInterface2) { t.Errorf("interface listener triggerd on non-matching type") })
	Dispatch(testEvent1{})

	if !triggered {
		t.Errorf("interface listener failed to trigger")
	}
}

func TestEmptyInterfaceListener(t *testing.T) {
	clearListeners()

	triggered := false
	AddListener(func(interface{}) { triggered = true })
	Dispatch("this should match interface{}")

	if !triggered {
		t.Errorf("interface{} listener failed to trigger")
	}
}

func TestMultipleListeners(t *testing.T) {
	clearListeners()

	triggered1, triggered2 := false, false
	AddListener(func(testEvent1) { triggered1 = true })
	AddListener(func(testEvent1) { triggered2 = true })
	Dispatch(testEvent1{})

	if !triggered1 || !triggered2 {
		t.Errorf("not all matching listeners triggered")
	}
}

func TestBadListenerWrongInputs(t *testing.T) {
	clearListeners()

	defer func() {
		err := recover()

		if err == nil {
			t.Errorf("bad listener func failed to trigger panic")
		}

		if _, ok := err.(BadListenerError); !ok {
			panic(err) // this is not the error we were looking for; re-panic
		}
	}()

	AddListener(func() {})
	Dispatch(testEvent1{})
}

func TestBadListenerWrongType(t *testing.T) {
	clearListeners()

	defer func() {
		err := recover()

		if err == nil {
			t.Errorf("bad listener func failed to trigger panic")
		}

		if _, ok := err.(BadListenerError); !ok {
			panic(err) // this is not the error we were looking for; re-panic
		}
	}()

	AddListener("this is not a function")
	Dispatch(testEvent1{})
}

func TestAsynchronousDispatch(t *testing.T) {
	clearListeners()

	triggered := make(chan bool)
	AddListener(func(testEvent1) { triggered <- true })
	go Dispatch(testEvent1{})

	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Errorf("asynchronous dispatch failed to trigger listener")
	}
}
