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

// MintInstanceID returns a new 26-character Crockford-base32 ULID encoded
// in lowercase. The mutating webhook and the controller fallback both
// call this so an admission-bypassed create still persists the same
// encoding the engine consumes as the metadata account ID.
func MintInstanceID() string {
	return strings.ToLower(ulid.MustNew(ulid.Now(), rand.Reader).String())
}
