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

package v1alpha1

import (
	"crypto/rand"
	"strings"

	"github.com/oklog/ulid/v2"
)

// CanonicalInstanceIDImageFloor is the engine and metadata tag at which
// lowercase FireboltInstance.spec.id is required. Empty means the floor
// is not published: MintInstanceID returns the uppercase Crockford
// encoding current images consume, and the controller leaves existing
// CRs unchanged.
var CanonicalInstanceIDImageFloor string

// MintInstanceID returns a new 26-character Crockford-base32 ULID. While
// CanonicalInstanceIDImageFloor is empty it returns the uppercase
// encoding current engine and metadata images consume as the account
// ID. Once the floor is set it returns lowercase, matching the
// encoding those images require. The mutating webhook and the
// controller fallback both call this so an admission-bypassed create
// still persists the same encoding.
func MintInstanceID() string {
	id := ulid.MustNew(ulid.Now(), rand.Reader).String()
	if CanonicalInstanceIDImageFloor == "" {
		return id
	}
	return strings.ToLower(id)
}
