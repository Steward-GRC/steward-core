// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
)

func TestEveryGRPCRPCIsImplemented(t *testing.T) {
	// RPCs deliberately left unimplemented, keyed "Service.Method", each with
	// its reason. They are logged, so a deferral is never silent.
	knownDeferred := map[string]string{}

	ctxT := reflect.TypeFor[context.Context]()

	entries := []struct {
		service string
		ifaceT  reflect.Type
		srv     reflect.Value
	}{
		{
			service: "AppendixService",
			ifaceT:  reflect.TypeFor[corev1.AppendixServiceServer](),
			srv:     reflect.ValueOf(corev1.AppendixServiceServer(&AppendixHandler{})),
		},
		{
			service: "AssetService",
			ifaceT:  reflect.TypeFor[corev1.AssetServiceServer](),
			srv:     reflect.ValueOf(corev1.AssetServiceServer(&AssetHandler{})),
		},
		{
			service: "EmailServiceSecretService",
			ifaceT:  reflect.TypeFor[corev1.EmailServiceSecretServiceServer](),
			srv:     reflect.ValueOf(corev1.EmailServiceSecretServiceServer(&EmailServiceSecretHandler{})),
		},
		{
			service: "DefinitionLibraryService",
			ifaceT:  reflect.TypeFor[corev1.DefinitionLibraryServiceServer](),
			srv:     reflect.ValueOf(corev1.DefinitionLibraryServiceServer(&DefinitionLibraryHandler{})),
		},
		{
			service: "ReferenceService",
			ifaceT:  reflect.TypeFor[corev1.ReferenceServiceServer](),
			srv:     reflect.ValueOf(corev1.ReferenceServiceServer(&ReferenceHandler{})),
		},
		{
			service: "CategoryService",
			ifaceT:  reflect.TypeFor[corev1.CategoryServiceServer](),
			srv:     reflect.ValueOf(corev1.CategoryServiceServer(&CategoryHandler{})),
		},
		{
			service: "SettingsService",
			ifaceT:  reflect.TypeFor[corev1.SettingsServiceServer](),
			srv:     reflect.ValueOf(corev1.SettingsServiceServer(&SettingsHandler{})),
		},
		{
			service: "ContactService",
			ifaceT:  reflect.TypeFor[corev1.ContactServiceServer](),
			srv:     reflect.ValueOf(corev1.ContactServiceServer(&ContactHandler{})),
		},
		{
			service: "RelationService",
			ifaceT:  reflect.TypeFor[corev1.RelationServiceServer](),
			srv:     reflect.ValueOf(corev1.RelationServiceServer(&RelationHandler{})),
		},
		{
			service: "TemplateService",
			ifaceT:  reflect.TypeFor[corev1.TemplateServiceServer](),
			srv:     reflect.ValueOf(corev1.TemplateServiceServer(&TemplateHandler{})),
		},
		{
			service: "PolicyService",
			ifaceT:  reflect.TypeFor[corev1.PolicyServiceServer](),
			srv:     reflect.ValueOf(corev1.PolicyServiceServer(&PolicyHandler{})),
		},
	}

	totalChecked := 0
	for _, e := range entries {
		t.Run(e.service, func(t *testing.T) {
			checked := 0
			for m := range e.ifaceT.Methods() {
				mt := m.Type
				// Unary RPCs only.
				if mt.NumIn() != 2 || mt.NumOut() != 2 || !mt.In(0).Implements(ctxT) || mt.In(1).Kind() != reflect.Pointer {
					continue
				}
				name := m.Name
				checked++
				totalChecked++
				key := e.service + "." + name
				if reason, ok := knownDeferred[key]; ok {
					t.Logf("RPC %s intentionally deferred (not implemented): %s", key, reason)
					continue
				}
				t.Run(name, func(t *testing.T) {
					defer func() {
						// A zero handler panics on its nil dependencies
						// as soon as real code runs; only the generated
						// stub returns cleanly. A panic is a pass.
						_ = recover()
					}()
					args := []reflect.Value{
						reflect.ValueOf(context.Background()),
						reflect.New(mt.In(1).Elem()),
					}
					out := e.srv.MethodByName(name).Call(args)
					if errv := out[1]; !errv.IsNil() {
						if err, ok := errv.Interface().(error); ok && status.Code(err) == codes.Unimplemented {
							t.Errorf("RPC %s is not implemented: it returns codes.Unimplemented (still served by the embedded Unimplemented%sServer stub). Add a real handler method, or add it to knownDeferred with a reason.", key, e.service)
						}
					}
				})
			}
			if checked == 0 {
				t.Fatalf("expected to check at least one RPC on %s, invoked 0 — reflection/filter is wrong", e.service)
			}
		})
	}

	// Guards against the reflection silently checking nothing.
	const minExpectedRPCs = 70
	if totalChecked < minExpectedRPCs {
		t.Fatalf("expected to check at least %d RPCs across all grpcsvc handlers, only invoked %d — reflection/filter is wrong or a service is missing from the table", minExpectedRPCs, totalChecked)
	}
}
