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

import (
	"fmt"
	"strings"
)

// gatewayPreStopScript fails the health check, holds admission open for
// propagationSeconds while the Service stops routing to this Pod, then drains
// the query listener and returns once no downstream connection is left.
// Returning lets kubelet send SIGTERM; a drained Envoy does not exit on its
// own. Admin failures retain the process until the Pod deadline. The image
// already supplies bash, cat, grep and sleep, so no extra client is needed.
//
// The propagation floor is load-bearing: the EndpointSlice
// controller and kube-proxy need on the order of a second to stop sending new
// connections to a terminating Pod, and the drain below stops the INBOUND
// listener as soon as it completes. Draining first, as an earlier version of
// this hook did, refused or black-holed every connection that arrived in that
// window. A Pod with no open connections would otherwise exit within
// milliseconds of preStop starting.
//
// The script keys on gatewayQueryListenerName / gatewayQueryStatPrefix so it
// cannot drift from the rendered envoy.yaml; the render test pins both.
func gatewayPreStopScript(adminPort int32, propagationSeconds int) string {
	statGauge := fmt.Sprintf("http.%s.downstream_cx_active", gatewayQueryStatPrefix)
	statGaugeRe := strings.ReplaceAll(statGauge, ".", "[.]")
	return fmt.Sprintf(`set -u
admin() {
  local response status
  response=$(
    exec 3<>/dev/tcp/127.0.0.1/%[1]d || exit 1
    printf '%%s\r\n' "$1 $2 HTTP/1.1" 'Host: localhost' 'Content-Length: 0' 'Connection: close' '' >&3 || exit 1
    cat <&3
  ) || return 1
  status=${response%%%%$'\n'*}
  [[ "$status" == HTTP/1.*" 200 "* ]] || return 1
  printf '%%s\n' "$response"
}
until admin POST /healthcheck/fail >/dev/null; do sleep 0.1; done
# Readiness is now failing. Keep accepting until the Service has stopped
# routing here; only then close admission. See the propagation note above.
sleep %[5]d
# graceful: in-flight requests finish and their connections close right after
# (drain-time-s is 0). The drain's completion also stops the INBOUND-marked
# listeners, so the query socket refuses new connections while the stats
# listener (not INBOUND) keeps serving probes and metrics. Verified against
# the pinned Envoy: adding skip_exit suppresses that listener stop and the
# socket keeps accepting, and without it the process still survives the
# drain - so skip_exit must stay absent.
until admin POST '/drain_listeners?inboundonly&graceful' >/dev/null; do sleep 0.1; done
while true; do
  if response=$(admin GET '/stats?filter=^%[2]s$'); then
    if grep -Fxq '%[3]s: 0' <<<"$response"; then
      exit 0
    fi
    # TLS provisioning can deliberately omit the public listener entirely.
    # Missing statistics alone cannot establish that there are no connections.
    if ! grep -q '^%[2]s:' <<<"$response"; then
      if listeners=$(admin GET /listeners) && ! grep -q '^%[4]s::' <<<"$listeners"; then
        exit 0
      fi
    fi
  fi
  sleep 0.1
done
`, adminPort, statGaugeRe, statGauge, gatewayQueryListenerName, propagationSeconds)
}
