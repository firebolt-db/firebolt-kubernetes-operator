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

package controller

import "fmt"

// gatewayPreStopScript closes public query admission before observing drained
// connections. Returning lets kubelet send SIGTERM; listener draining itself
// does not exit Envoy. Admin failures retain the process until the Pod deadline.
// The image already supplies bash, cat and grep, so no extra client is needed.
func gatewayPreStopScript(adminPort int32) string {
	return fmt.Sprintf(`set -u
admin() {
  local response status
  response=$(
    exec 3<>/dev/tcp/127.0.0.1/%d || exit 1
    printf '%%s\r\n' "$1 $2 HTTP/1.1" 'Host: localhost' 'Content-Length: 0' 'Connection: close' '' >&3 || exit 1
    cat <&3
  ) || return 1
  status=${response%%%%$'\n'*}
  [[ "$status" == HTTP/1.*" 200 "* ]] || return 1
  printf '%%s\n' "$response"
}
until admin POST /healthcheck/fail >/dev/null; do sleep 0.1; done
until admin POST '/drain_listeners?inboundonly&graceful' >/dev/null; do sleep 0.1; done
while true; do
  if response=$(admin GET '/stats?filter=^http[.]gateway[.]downstream_cx_active$'); then
    if grep -Fxq 'http.gateway.downstream_cx_active: 0' <<<"$response"; then
      exit 0
    fi
    # TLS provisioning can deliberately omit the public listener entirely.
    # Missing statistics alone cannot establish that there are no connections.
    if ! grep -q '^http[.]gateway[.]downstream_cx_active:' <<<"$response"; then
      if listeners=$(admin GET /listeners) && ! grep -q '^listener::' <<<"$listeners"; then
        exit 0
      fi
    fi
  fi
  sleep 0.1
done
`, adminPort)
}
