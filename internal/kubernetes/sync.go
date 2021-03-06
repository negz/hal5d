/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package kubernetes

import (
	"k8s.io/client-go/tools/cache"
)

type resourceEventer interface {
	ResourceEvent()
}

type add struct {
	obj interface{}
}

func (e add) ResourceEvent() {}

type update struct {
	oldObj, newObj interface{}
}

func (e update) ResourceEvent() {}

type delete struct {
	obj interface{}
}

func (e delete) ResourceEvent() {}

// A SynchronousResourceEventHandler forwards all events
type SynchronousResourceEventHandler struct {
	event chan resourceEventer
	h     cache.ResourceEventHandler
}

// NewSynchronousResourceEventHandler returns a new ResourceEventHandler which
// passes all handled events to the supplied ResourceEventHandler synchronously.
// Events are buffered. When the the supplied buffer size is exhausted the
// handler will block.
func NewSynchronousResourceEventHandler(h cache.ResourceEventHandler, buffer int) *SynchronousResourceEventHandler {
	return &SynchronousResourceEventHandler{event: make(chan resourceEventer, buffer), h: h}
}

// Run until the provided stop channel is closed.
func (b *SynchronousResourceEventHandler) Run(stop <-chan struct{}) {
	for {
		select {
		case e := <-b.event:
			b.forward(e)
		case <-stop:
			return
		}
	}
}

func (b *SynchronousResourceEventHandler) forward(e resourceEventer) {
	switch e := e.(type) {
	case *add:
		b.h.OnAdd(e.obj)
	case *update:
		b.h.OnUpdate(e.oldObj, e.newObj)
	case *delete:
		b.h.OnDelete(e.obj)
	}
}

// OnAdd forwards notifications of new resources.
func (b *SynchronousResourceEventHandler) OnAdd(obj interface{}) {
	b.event <- &add{obj}
}

// OnUpdate forwards notifications of updated resources.
func (b *SynchronousResourceEventHandler) OnUpdate(oldObj, newObj interface{}) {
	b.event <- &update{oldObj, newObj}
}

// OnDelete forwards notifications of deleted resources.
func (b *SynchronousResourceEventHandler) OnDelete(obj interface{}) {
	b.event <- &delete{obj}
}
