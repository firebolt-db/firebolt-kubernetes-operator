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
// lowercase FireboltInstance.spec.id is required: the first packdb build
// that reads spec.id as the metadata account ID in the lowercase
// Crockford encoding. It matches the ENGINE_TAG / METADATA_TAG pins in
// config/images/defaults.latest.env, and both must move together — an
// image bump that leaves the floor behind stops canonicalizing ids on
// clusters that are already on the new build.
//
// Empty means the floor is not published: MintInstanceID returns the
// uppercase Crockford encoding older images consume, and the controller
// leaves existing CRs unchanged. Tests clear it to exercise that path.
var CanonicalInstanceIDImageFloor = "release-5.0.0-pre.0.20260828194119.d0f954993097"

// MintInstanceID returns a new 26-character Crockford-base32 ULID in the
// lowercase encoding engine and metadata images consume as the account
// ID. While CanonicalInstanceIDImageFloor is empty it returns the
// uppercase encoding instead, which is what images below the floor
// consume. The mutating webhook and the controller fallback both call
// this so an admission-bypassed create still persists the same
// encoding.
func MintInstanceID() string {
	id := ulid.MustNew(ulid.Now(), rand.Reader).String()
	if CanonicalInstanceIDImageFloor == "" {
		return id
	}
	return strings.ToLower(id)
}
