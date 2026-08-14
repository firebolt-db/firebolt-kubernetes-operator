# Changelog

## [0.8.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.8.0...firebolt-operator-v0.8.1) (2026-08-14)


### Bug Fixes

* **api:** let sidecars and init containers mount non-Secret operator volumes (FB-3075) ([9038f4f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9038f4f3dd04cecccfc9bffde370971bc6357bdb))

## [0.8.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.7.1...firebolt-operator-v0.8.0) (2026-08-14)


### Features

* **controller:** add --watch-label-selector to scope cached Firebolt CRs (FB-3039) ([2921fb0](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2921fb09cdd95b1c4c68e686f293b46111436c55))

## [0.7.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.7.0...firebolt-operator-v0.7.1) (2026-08-13)


### Bug Fixes

* **controller:** anchor the generation floor to the engine's own status (FB-3011) ([c3a5e99](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c3a5e991d9d1db4b5b7e31071c9df03eb78321c7))
* **controller:** build the whole keep set from one view of the cluster (FB-3011) ([0fd716c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/0fd716cfbab64118c413577cfe25fb6310b4940d))
* **controller:** end the pass when the engine status cannot be read (FB-3011) ([bcde48d](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/bcde48d86bf2f8300900cbf8da4120e1ce10f7cc))
* **controller:** never sweep a generation newer than the keep set (FB-3011) ([80b9a21](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/80b9a2155f23c06d24c82d3eb919b095def28ad4))

## [0.7.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.6.1...firebolt-operator-v0.7.0) (2026-08-13)


### Features

* add experimental metadata-ng mode (FB-3014) ([7556006](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/755600618ed6d52a850cd19bbc6493f5f1e95cb8))


### Bug Fixes

* **controller:** defer the sweep so every pass reclaims (FB-3011) ([1499f57](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/1499f5795067e47ea7c6a3ae2c6e7839540eeb32))
* **controller:** delete only what the engine owns and what nothing serves (FB-3011) ([12e026f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/12e026f0715e50db5aad75b30eef3607a0b157ed))
* **controller:** reclaim abandoned generations in every phase (FB-3011) ([60f4675](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/60f467550b4286b7f7c5deaf37fde660d845ee0f))
* **controller:** report a sweep backlog on read failures and mixed requeues (FB-3011) ([8eb9f30](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8eb9f3042b594c0828baa876b0d7c9ca619ea26d))
* **controller:** skip the sweep on a panic unwind, keep immediate requeues (FB-3011) ([215775b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/215775b03ff307ea652955c3eaf947cd41fcbdb1))
* **controller:** sweep ahead of the reconcile gates (FB-3011) ([a4878f2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a4878f25ced7242863f0f75cfe6990c0762124c1))

## [0.6.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.6.0...firebolt-operator-v0.6.1) (2026-08-12)


### Bug Fixes

* **controller:** preflight gateway CRL secrets without draining (FB-2928) ([#124](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/124)) ([3ebae32](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3ebae327145569e9175a4de3766f69f605cfac68))
* **controller:** refuse HTTP redirects on pod-IP scrape clients (FB-2668) ([#122](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/122)) ([979ddcd](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/979ddcd1ddde7f58d66a1fa32c3a12ed1f576cd3))
* **controller:** roll gateway on CRL secret updates (FB-2667) ([#123](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/123)) ([1866493](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/1866493c1df0e1464126ac1ee56f8765901b046b))
* **controller:** surface gateway CRL without client CA when webhooks off (FB-2929) ([#126](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/126)) ([4cb763c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4cb763c0a719e52280029f0711378424c67b1863))
* **metadata:** render dedicated-pensieve config as YAML, not XML (FB-2877) ([#120](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/120)) ([6fabbb5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6fabbb5fbe94cc19ee88991b6a494c0c32a97873))


### Dependencies

* **deps:** bump github.com/cert-manager/cert-manager from 1.20.3 to 1.21.0 ([#116](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/116)) ([2ce757d](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2ce757d3f4b4d5ee9b7c95b8d6d4ec4bd62a2983))
* **deps:** bump github.com/cert-manager/cert-manager from 1.21.0 to 1.21.1 ([#129](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/129)) ([b2fd401](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b2fd401155fe118980d30bbdd56f0d1796fce379))
* **deps:** bump github.com/go-logr/logr from 1.4.3 to 1.4.4 ([#117](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/117)) ([d57c786](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d57c786789d8f1ed112a54bfb9fc1d41d1900134))
* **deps:** bump github.com/oklog/ulid/v2 from 2.1.1 to 2.1.2 ([#115](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/115)) ([01469c5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/01469c51ecad98ac7aafbe1d9faca7c1d3e27663))
* **deps:** bump github.com/prometheus/client_golang from 1.23.2 to 1.24.1 in the prometheus group across 1 directory ([#114](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/114)) ([c3abba2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c3abba2c40c572c1547c1a2ca316d8dc596226f9))
* **deps:** bump packdb engine/metadata to 5.0.0-pre.0.20260803130231.8ec167d19785 ([40df5e3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/40df5e311bb2df4a888e581037c4a8e2e3b39898))
* **deps:** bump packdb engine/metadata to 5.0.0-pre.0.20260808171517.d7585f66d216 ([#127](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/127)) ([2a67b1f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2a67b1f1c71de2473ff796f41a6b9e7a2f2553fa))
* **deps:** bump the kubernetes group across 1 directory with 4 updates ([#113](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/113)) ([7a2a42c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7a2a42c8a494abe359e5f2b0092163a8c5953994))

## [0.6.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.5.1...firebolt-operator-v0.6.0) (2026-07-28)


### Features

* **cmd:** run the wake agent as a manager subcommand (FB-2553) ([36e8973](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/36e8973d2316d27e0ab9882d95170582dde7080d))
* **controller:** wake stopped engines from gateway demand (FB-2553) ([7439de9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7439de9f5766f88be58ca2e7bd7cf8bafa39e855))
* **gateway:** add a read-only wake agent for stopped-engine queries (FB-2553) ([5699b28](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/5699b287b5980273833337277cea6675038656d0))


### Bug Fixes

* **controller:** say wake-on-zero is off with a user gateway ServiceAccount (FB-2553) ([6134029](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/613402994723426b193ad343f4babc19b784c57b))
* **formal:** give mktemp a template so the mutant check runs on macOS (FB-2582) ([6c433c5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6c433c5e38766a04051a90eb5730a9905f8fb8a5))
* **formal:** never leave a mutant applied when the check aborts (FB-2582) ([db6a74e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/db6a74efee32cfda63fed3431e601206bba95071))
* **formal:** pin the cleaning mutant by hazard, not by sort order (FB-2583) ([40772e2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/40772e22cb70a7ef1599337049a3f4790f7557b8))
* **formal:** stop re-downloading tla2tools on every formal target (FB-2609) ([8ffcc06](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8ffcc066cac4895663d11efd0d871977581b31db))
* **gateway:** bound the demand tracker at a hard engine-name cap (FB-2553) ([e83154e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e83154e147f716092c14e12b1a9ae812f1cd0396))
* **gateway:** key waiter release on channel identity, not engine name (FB-2553) ([340706f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/340706f53e55d36f031157f646ba5ea3a66b8886))
* **gateway:** stop telling users to grant engine write RBAC (FB-2553) ([6390984](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/63909842088aeed3c223b587249dda697b95894d))
* **gateway:** stop the wake agent from being able to take the gateway down (FB-2553) ([134b27c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/134b27c6169a870f4206d2c776158f14fd314666))

## [0.5.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.5.0...firebolt-operator-v0.5.1) (2026-07-27)


### Bug Fixes

* **security:** harden auth & TLS secret, certificate, and admission guards (FB-896) ([#89](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/89)) ([e7794ee](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e7794ee489ec149d686c0fbdf768422cf3927e80))


### Dependencies

* **deps:** bump github.com/google/cel-go from 0.26.0 to 0.29.0 ([#88](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/88)) ([6e5040d](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6e5040d5ea74a07c853a766caeb4ae0812e97862))

## [0.5.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.4.1...firebolt-operator-v0.5.0) (2026-07-27)


### Features

* **api:** add spec.auth and spec.tls to FireboltInstance with validation and RBAC (FB-896) ([59f792b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/59f792b43c2dd2b59c3be11a9310f392d046dad0))
* **controller:** provision auth, TLS, and coordinated signing-key rotation (FB-896) ([63d000a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/63d000aced7d64273dab54bd68b2aa36029caa81))


### Bug Fixes

* **cli:** derive the port-forward scheme from observed TLS state (FB-896) ([9e8a5a3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9e8a5a3a0476506a301a785e16c7a63ece81f4e8))
* **controller:** disable service account token automount on postgres (FB-2516) ([e0ad19e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e0ad19eeb7824b5600749cbbbfd804f53a4193f7))
* **gateway:** offer every engine-certificate curve on the upstream TLS context (FB-896) ([e5d36e3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e5d36e32bb3b32c3c370752f406a235af32e9acd))


### Dependencies

* **deps:** bump golang from 1.26.4 to 1.26.5 ([#85](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/85)) ([75f008e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/75f008e15aecd6f880d8bcbbcdcce3356166046f))
* **deps:** bump packdb engine/metadata to 5.0.1-0.20260727005216.d09b51086f14 ([96d5881](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/96d58814a48268778ca207ff6e9b1fced7a0c9ae))

## [0.4.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.4.0...firebolt-operator-v0.4.1) (2026-07-21)


### Bug Fixes

* **controller:** stop injecting core mode env (FB-1860) ([#74](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/74)) ([7f912e6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7f912e68e9678795d3e9bcf7192cc1653713acbb))

## [0.4.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.3.2...firebolt-operator-v0.4.0) (2026-07-20)


### Features

* add anonymous Scarf usage telemetry (FB-1354) ([86ac31b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/86ac31bb83f8c58cc338274149d17ee3a4c3aac8))
* couple telemetry opt-out to default image routing (FB-1354) ([3d78f21](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3d78f218a2cb035917a961e5dd41164c2d5e2ac5))
* route public operator and engine pulls through Scarf (FB-1354) ([3b2a64f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3b2a64f50619ae2ed5707737322e16aafc33bee7))


### Bug Fixes

* address telemetry scheduling and Kind routing review (FB-1354) ([abdc68b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/abdc68bca137f723842240dfb4efa68157369310))
* **controller:** render the GC config keys the metadata server actually reads (FB-2259) ([#65](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/65)) ([8e7c9f1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8e7c9f15f72fb7f7e93c221ae2cb45fc3209c003))

## [0.3.2](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.3.1...firebolt-operator-v0.3.2) (2026-07-14)


### Bug Fixes

* **ci:** harden sync-chart-appversion against main push races (FB-2030) ([6ae5003](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6ae5003d826a623eef22981adfeecc9945f85be7))
* **controller:** add readiness probe to the engine-web UI sidecar (FB-2175) ([805e789](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/805e789fd8a78d4196f19e0494accffc6334d0fb))
* **controller:** normalize probe API-server defaults in container drift comparison (FB-2175) ([93cbdc5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/93cbdc569615ff3524c3e5c5ee799bba94720527))

## [0.3.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.3.0...firebolt-operator-v0.3.1) (2026-07-14)


### Bug Fixes

* **controller:** follow Kubernetes tag-based image pull-policy defaults (FB-2172) ([00900e5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/00900e509ae4ba4d20ae898b57021a7d92facee5))

## [0.3.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.2.1...firebolt-operator-v0.3.0) (2026-07-13)


### ⚠ BREAKING CHANGES

* **storage:** the generated config requires a post-FB-1684 engine (packdb #23716). The operator ships version-matched with the engine, so this is the release boundary rather than an in-place break.

### Features

* **storage:** emit FB-1684 managed-table storage schema in kubectl-firebolt and the builder (FB-1684) ([2c031e9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2c031e985e65b2ee4fa66c5c7826a086cce6a60a))


### Dependencies

* **deps:** bump golang.org/x/net from 0.53.0 to 0.55.0 ([#43](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/43)) ([bb7ca12](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/bb7ca1238b09bf9c2406fbafc34de56d090deeee))
* **deps:** bump packdb engine/metadata to 5.0.1-0.20260709071413.53735f172429 ([41d5fba](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/41d5fbad51a81ccf7a46ddcd4f08279fe7ef126a))
* **deps:** bump packdb engine/metadata to 5.0.1-0.20260713060957.513515666721 ([#49](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/49)) ([64e14f3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/64e14f34ba9ee05e7c8c16c02f9de5bf3e059d69))
* **deps:** bump the ginkgo-gomega group across 1 directory with 2 updates ([#41](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/41)) ([6c9b2c8](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6c9b2c881124fe4a5d29d8850c268304ee7b12e7))

## [0.2.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.2.0...firebolt-operator-v0.2.1) (2026-06-24)


### Dependencies

* **deps:** bump github.com/onsi/ginkgo/v2 from 2.29.0 to 2.31.0 ([#21](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/21)) ([3c59616](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3c59616d9b4aa8f119c3948392284acb39c13ee9))
* **deps:** bump github.com/onsi/gomega from 1.41.0 to 1.42.0 ([#19](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/19)) ([dec29e6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/dec29e64f3547083df762e9a27c246ad8519c2b7))
* **deps:** bump k8s.io/api from 0.36.1 to 0.36.2 ([#20](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/20)) ([bc15667](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/bc15667a97851e7e86d4a5b7778019f68626b386))
* **deps:** bump k8s.io/apiextensions-apiserver from 0.36.1 to 0.36.2 ([#17](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/17)) ([50781ca](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/50781cac6213e54da0497d0eb3e31e0cbaee3cd2))
* **deps:** bump k8s.io/apimachinery from 0.36.1 to 0.36.2 ([#18](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/18)) ([a519183](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a51918309bed3a23c031d3a716882e15b2f33af6))
* **deps:** bump k8s.io/client-go from 0.36.1 to 0.36.2 ([#16](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/16)) ([01535d1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/01535d1a5b159499ecc04e557dfeb654b4cce564))

## [0.2.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-v0.1.1...firebolt-operator-v0.2.0) (2026-06-22)


### ⚠ BREAKING CHANGES

* **controller:** align with engine FHS image layout (FB-1733) ([22cd3d2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/22cd3d2d694390352c1ea7bad42e31c4c5c5ba9e))

## [0.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v0.0.9...v0.1.0) (2026-06-12)

### Features

* set new version ([#9](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/9)) ([9b58ce1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9b58ce134691e1ade661a7d680dfec5018bad6db))
