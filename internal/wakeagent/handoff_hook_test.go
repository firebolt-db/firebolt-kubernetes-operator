//go:build !wakehandofftest

/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wakeagent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalBuildHasNoHandoffControl(t *testing.T) {
	a := testAgent(t, time.Second)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		a.demandMux().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/test/handoff?engine=engine", http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("test control exposed in normal build: %d", rec.Code)
		}
	}
}
