# 0.1.9

appVersion: v1.6.0

## [1.6.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.5.1...v1.6.0) (2026-04-21)


### Features

* **helm:** add CRD chart and move operator CRDs to crds/ directory (FB-553) ([1de6146](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1de61469d45c36653e130ac64cdaef75240df3a3))


### Maintenance

* **helm:** bump chart to 0.1.8 (appVersion v1.5.1) ([2890afe](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2890afeaa71b55a59f0c659844c8277cb2466335))
* **helm:** regenerate READMEs with helm-docs v1.14.2 footer (FB-553) ([97b0f32](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/97b0f32ea86afc9bf29435f1d33436e4dbb7a085))


### Bug fixes

* **ci:** add CRD chart to helm-template and fix early-exit version paths (FB-553) ([fb43031](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/fb430311daac8df90710e610a0a827d0c3084b71))
* **ci:** match commit guard to plural format and skip changelog on CRD-only bumps (FB-553) ([38f69e6](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/38f69e626b0c6484044e6ef70a44220fabd37ec7))
* **helm:** add empty map to CRD chart values.yaml for helm-docs (FB-553) ([95128a2](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/95128a226787b2b99dda49b198decb10d9f5cd24))



# 0.1.8

appVersion: v1.5.1

### [1.5.1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.5.0...v1.5.1) (2026-04-21)


### Bug fixes

* **docker:** include go:embed defaults.env in the CI build context ([2cec4cc](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2cec4ccc2c443de925e5995c92ffd89f46426f8c))


### Maintenance

* **helm:** bump chart to 0.1.7 (appVersion v1.5.0) ([27b5e58](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/27b5e58d1220b4e52200e9ab39db86c452606c60))
* **helm:** regenerate README after chart bumps 0.1.5 -> 0.1.7 (FB-553) ([a1f26d5](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/a1f26d5600ce0dbea1f95d7b6d75621560dbe478))



# 0.1.7

appVersion: v1.5.0

## [1.5.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.4.0...v1.5.0) (2026-04-21)


### Features

* **api:** allow spec.replicas=0 and introduce Stopped phase (FB-555) ([5adcca0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/5adcca0a495abbc0c88673ec9310ed65cb75a9ff))
* **controller:** route PhaseStopped through state machine and Ready condition (FB-555) ([cc0de25](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/cc0de2529173ec2657fe91c8c3340d7311f7a4ac))
* **formal:** extend TLA+ spec with Stopped phase and verify scale-to-zero (FB-555) ([411e133](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/411e133e4ddd44c0f09d2d1492fa7480ea66afe8))


### Maintenance

* **helm:** bump chart to 0.1.6 (appVersion v1.4.0) ([7cbfb25](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7cbfb259b7e14a51a301a0a6352859ece9d3d17c))


### Bug fixes

* **controller:** extend terminal-phase invariant panic to Stopped (FB-555) ([472d727](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/472d727cdbc71e7b5eec0e616e5357fa76ed71d5))



# 0.1.6

appVersion: v1.4.0

## [1.4.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.3.0...v1.4.0) (2026-04-21)


### Features

* **cd:** push operator image and helm chart to ECR (FB-553) ([a7998d8](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/a7998d81d0e0dc27841a3f10de609f60073bad9a))


### Maintenance

* **helm:** bump chart to 0.1.5 (appVersion v1.3.0) ([bf6ebd4](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/bf6ebd42c4dc112c450e2ace236b4cbea9027bc2))



# 0.1.5

appVersion: v1.3.0

## [1.3.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.2.1...v1.3.0) (2026-04-20)


### Features

* add CLAUDE.md (FB-700) ([c45ae2f](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c45ae2fef3fbb3aba2885220fe3f8278034abab2))
* add GH workflow to run TLC (FB-700) ([f5678c9](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/f5678c913be5798d4e036ee122fbdb134645b00e))
* add TLA+ spec (FB-700) ([d92662e](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/d92662e0e56261dbe2b8c0240d3d1071d0c0e433))


### Maintenance

* **helm:** bump chart to 0.1.4 (appVersion v1.2.1) ([bb659ea](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/bb659ea16ec6c19e949f64e2a260d35d07b13167))


### Bug fixes

* **formal:** correct TLC violations in FireboltEngine spec (FB-700) ([1010410](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/101041045a1f27778fb463dff3a45faa735b3724))
* **formal:** fix two more TLC violations in FireboltEngine spec (FB-700) ([254cf83](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/254cf834cfc8ef1bdd79f9c5aa0c341e8fd9a75f))
* **formal:** reset podsReady in SpecDrift_AtMax action (FB-700) ([3284470](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/3284470853489a55acaa3bd4511718ee0a34fd0a))
* use latest Core image (FB-700) ([db42176](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/db421766c42170a6c7677bae083f7d969e84b6c4))



# 0.1.4

appVersion: v1.2.1

### [1.2.1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.2.0...v1.2.1) (2026-04-20)


### Maintenance

* **envoy:** bump to v1.37.2 (FB-736) (#20) ([996ebb9](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/996ebb9294c809bf85bf2699bb05fad5366015ad))
* **helm:** bump chart to 0.1.3 (appVersion v1.2.0) ([2303ccb](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2303ccb3ef01aa89ab5e37624316bac474bcdaf6))



# 0.1.3

appVersion: v1.2.0

## [1.2.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.1.0...v1.2.0) (2026-04-17)


### Features

* add script to do emergency cleanup (FB-661) ([e4ca91b](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e4ca91b5d369a05dc8ae058795f95ae4c74836f1))
* **api,controller:** add FireboltEngine ConditionReady roll-up (FB-661) ([2af74c7](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2af74c7fd13eafb5560faab1410fe85bc75b5c9a))
* **api,controller:** validate external Postgres credentials Secret (FB-661) ([de9664f](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/de9664f4a57970db579d47b222f423c11ca22e49))
* **api:** add spec.terminationGracePeriodSeconds to FireboltEngine (FB-661) ([e079947](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e0799472d81f1b2457ae9d38a0512cfb3b70b10d))
* **api:** enforce immutability of instanceRef and instance ID via CEL (FB-661) ([fed5b84](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/fed5b84bb48ce286c0b408c7a606854ae146b4cc))
* **api:** make FireboltEngine spec.metadataEndpointOverride immutable once set (FB-661) ([763334a](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/763334a1f24661b985fd7aa190e7b34db805102e))
* **api:** reject reserved firebolt.io/ keys in webhook validation (FB-661) ([b634c20](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/b634c206225d4bba6f8c90835f2d23c252f4370c))
* **controller:** add Conditions to FireboltInstance and propagate ensure errors (FB-661) ([2cb5959](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2cb5959045ba6faba0b8bc2d43c635cae7da81b2))
* **controller:** add generation GC sweep for orphaned resources (FB-661) ([c75a4eb](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c75a4ebed011a9cdcb015b46df50dcb695812ecb))
* **controller:** drain via Prometheus metrics + engine preStop hook (FB-661) ([de495ce](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/de495ce23f1cf35fca7ec946e53e05a8d916b2f3))
* **controller:** make engine cluster service headless and drop endpoint-ready gate (FB-661) ([da91dd8](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/da91dd80bd44a9bc9d15ccd6909891c65de9e4eb))
* run gingkgo tests in parallel (FB-661) ([6b5bde7](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6b5bde79c63489eec1135f2d531bf907243da840))
* switch to operator-maintained headless service (FB-661) ([e1ee4df](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e1ee4df26aee1086fc5fbeffc8937aa3a5a80c82))


### Maintenance

* **api:** resync FireboltEngine CRD description after comment reflow (FB-661) ([ae7b261](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/ae7b2616065c6e77f228f348691d287415909755))
* **cmd:** ignore fmt.Println error in --version branch (FB-661) ([4bcc3d0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/4bcc3d01649aeb0b1772b5f9b6559a9e7bd64401))
* **helm:** bump chart to 0.1.2 (appVersion v1.1.0) ([0acd8df](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/0acd8df9ba7ad6738603a0eda112d27841b218a8))


### Bug fixes

* add missing RBAC permissions (FB-661) ([4b4d95d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/4b4d95d114334917e17c6fa8865595e03c5b9d70))
* avoid running multiple E2E test runs (FB-661) ([aecb257](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/aecb25772c51b2023c759b2b4bb11259902c2ab5))
* **controller:** bound account-init flow with a 30s deadline (FB-661) ([1a6362c](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1a6362c6c6e83b7aea374ac038423a8c67a3974e))
* **controller:** load-balance gateway across engine pods via DFP sub-cluster mode (FB-661) ([dab80bd](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/dab80bd77c9480925488341f3b5a04b1fce23833))
* **controller:** preserve full entropy in generatePassword (FB-661) ([046ce64](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/046ce64d4fa1ba4372a0f0ef515da1f28d1d53a1))
* **controller:** report ready-vs-total pods in PodsNotReady message (FB-661) ([728a814](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/728a8147f57960a4a5eaaf58c2e997952a1ad6f4))
* **controller:** self-heal missing engine ConfigMap and headless service in place (FB-661) ([3d5977c](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/3d5977c7576726d5f235b1c33f785d492d09f12b))
* **controller:** skip generation-less resources in engine GC sweep (FB-661) ([9992570](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/9992570d382a10aed8cc21bbab2fba948e6aac26))
* **controller:** stop reconciling FireboltInstances stuck in Failed (FB-661) ([537e3ef](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/537e3efadcbdd5d06df70ea86338e47218cc18d3))
* **controller:** surface drain-probe failures via DrainCheckFailing condition (FB-661) ([e582815](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e582815b18f047f2c06ca23bc496dcf5f6ac6dea))
* **controller:** surface non-NotFound errors from engine state getters (FB-661) ([c1e4365](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c1e4365fea07ca2b8a3f8a76cd69be1f6ea6cbc5))
* **controller:** widen gateway shutdown budget and calm readiness probe (FB-661) ([18cec03](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/18cec03f02f8dd8950ed8019f632edb66df8275c))
* do not remove kind cluster (FB-661) ([500af04](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/500af0443d375ff4351f2fbbea860dd1b0967f98))
* **helm:** align ClusterRole with actual operator RBAC needs (FB-661) ([4693610](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/46936101b6abac0cd25e761da0c22826b09f919a))
* ignored errors (FB-661) ([80504f4](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/80504f4b32ff944f349001cb376fbfa80a7fe28a))
* make sure TGPS is checked (FB-661) ([3b14dc0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/3b14dc0e5e4647b77ea118b9669d48c601908c93))
* saner deployment target names (FB-661) ([b99e611](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/b99e6114da65d9805851c4ef1f4ce0335193032e))
* set license year and company (FB-661) ([e48e866](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e48e86607a40ae3059decd4289eb2147d0ff9e2e))



# 0.1.2

appVersion: v1.1.0

## [1.1.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.0.0...v1.1.0) (2026-04-17)


### Features

* **api:** add immutable spec.id with ULID defaulting webhook (FB-557) ([afcd2c3](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/afcd2c34f1dd5856a90507d6561d844452a1fbf7))
* **api:** make engine image optional with operator defaults (FB-557) ([ad596a9](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/ad596a99c09c8762e95598c97722748a3d2b70a9))
* **api:** restore auth spec on FireboltInstance CRD (FB-661) ([09fc1da](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/09fc1da314634c36468f7c6f4e42935042e83576))
* **controller:** replace core-gateway with Envoy proxy (FB-661) ([9f96fb7](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/9f96fb74bac18d1ed72ba64f65dda8ee9417b157))
* expose advanced mode (FB-557) ([5e1c851](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/5e1c85106fed19e5da2554af7d6e31b7268e28b9))
* parallelize kind image load (FB-557) ([6e32726](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6e32726f387b1cbaf4d139bc3eb45995ebfc8317))


### Maintenance

* **cursor:** add e2e rule for zero-downtime tests (FB-557) ([c061f9a](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c061f9aabe476d389ecab0af95eedfc7835bdfd9))
* **examples:** remove outdated engine example manifests (FB-557) ([926dc24](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/926dc24ca89fb8c157ad744e6bd92b126a979fdb))
* **helm:** bump chart to 0.1.1 (appVersion v1.0.0) ([9059920](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/90599206884abf85e3f835275d1e54d218f9e274))


### Bug fixes

* **controller:** abandon in-progress engine generation on spec drift (FB-557) ([ceda111](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/ceda111b5a283f3ef8787e3f961990500fedf647))
* **controller:** add advanced_mode query param for Core compatibility (FB-661) ([2419d20](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2419d2016ea3c1d883dc520d73b8faa7ba2643b7))
* **controller:** address review feedback across webhook and reconciler (FB-557) ([d059d1f](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/d059d1f9eeb49c90319f07f1911efd3c8b053f98))
* **controller:** fail fast on missing instance id and cache AccountReady (FB-557) ([97add03](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/97add038447b763156be478f60cf19b3e174363e))
* missing RBAC role (FB-557) ([863cef1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/863cef1080e4cc6a594bc127741e28243d705959))



# 0.1.1

appVersion: v1.0.0

## 1.0.0 (2026-04-16)


### Features

* add alternative design from 30 Dec 2025 ([7795dda](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7795ddaa5c45c1fc4d7727c66a94f1826b28a1e1))
* add crash recovery test suite ([515ced9](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/515ced90645fbb4092d3607313bf7fedee5b6647))
* add current dedicated-pensieve chart ([b982cc6](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/b982cc67cc494cc155f1233de85f24077f3272df))
* add FireboltInstance reconciler and account initialization (FB-571) (#6) ([14d358c](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/14d358c9eb30a29b29d51116d3791e574033cbac))
* add rollout strategy and heavy tests ([42306be](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/42306be1f904bb38bec38246d7b28fc16e7770e3))
* add separate cursor rule for build (FB-661) ([53cefc2](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/53cefc239b958271d7abd5126dd52c52f8dfde84))
* add support for account id ([7b862ee](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7b862eed78d68b42d204ab22d3312d8e19955508))
* add support for metadata service ([c970054](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c9700544861ed37f412b2c6ea7aec9a3fe021426))
* add test for image replacement, add more tests for multi-cluster handling and heavy queries ([6873c4d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6873c4dfa9836eb15598aa9b97676e87f1ac9d07))
* add test for validation ([92fe333](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/92fe33324d4c2cd98d4a2738f6234fa7d6f9c71f))
* adding E2E tests ([6241e1d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6241e1da16b8816dc951706177fd36740ab92e76))
* allow local deployment and iterations on Linux (FB-661) ([67e3d9d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/67e3d9ddb13031b0b450a964e0be58c4c5c4cc54))
* **api:** add webhook validation for metadata replicas (FB-661) ([6691bcd](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6691bcd203bde6715ada9a10cd89f887ded00e11))
* **cd:** add app release cd (FB-553) ([5e9a10a](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/5e9a10a010fb8bd94ffc75f6aca8625bbe4b5f75))
* **ci:** migrate Helm chart publishing from ECR to GHCR ([0435b92](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/0435b92fb78d11129d75061b9443b3164050e75c))
* **crd:** add instance CRD and improve build experience (FB-550) (#3) ([e35c6fb](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e35c6fbfd0c8b0274c895614466f367ca200c094))
* **engine:** level-driven reconciliation (FB-550) (#4) ([25a4463](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/25a44638208aad06ab5d39db9adc47f20fdc6898))
* first implementation ([7bb17db](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7bb17dbd94f97efdf45b84fbdbb83db6a25e96c4))
* **helm:** add Helm chart for the operator (FB-553) ([94a0e0b](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/94a0e0b990912b3eae2101913b7bfa05e2104129))
* **helm:** add README and bug fix (FB-553) ([4f02c75](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/4f02c75c533e28fb32b5cf7d62f70d33afb5387b))
* initial operator generation using kubebuilder ([1edcc3b](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1edcc3b958bafcc9b8dd66511fbedcbe5e0ea644))
* split tests (FB-661) ([d2d23da](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/d2d23da558e57123f90c394c2c574af26eb2aac3))
* use CRD, FireboltEngine ([eb99193](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/eb99193a4094df857bb7f56a722a1f3b6346d886))


### Maintenance

* add Apache 2.0 license (FB-679) (#17) ([13d8a20](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/13d8a205fbc224061684de460a253a21de9cd51e))


### Bug fixes

* avoid leak on N+1 crash (FB-571) (#5) ([b3045c1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/b3045c1fa982b7f0d1e0984b81f1cd94b3fa5631))
* bug with tests cleanup ([a3acef0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/a3acef06e5f1fc67c93353c0a741b64d209fb530))
* **cd:** use app id for autobump and client_id for push (FB-553) ([d0d8985](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/d0d89854d350adcce1d25129476d99626a8cca90))
* **ci:** add token for analytics org (FB-553) ([0551619](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/0551619188b6f25367a421a8af5606c351fb2cc6))
* **ci:** bug fix in helm-push (FB-553) ([39bdbcc](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/39bdbcca56b361b8b5f5d077918849011be6e0ad))
* **ci:** improve helm-push action (FB-553) ([961583e](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/961583e9db0351c28d5c4d3e8069f1fa1c2e7f95))
* E2E tests ([4e289a3](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/4e289a3f3095b5fec7b4f647b9900371960ea7f2))
* E2E tests (FB-661) (#9) ([91a748a](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/91a748a5d42c455e00a80f9caa0061bf312cd4ae))
* **helm:** accept numeric values in resource limits and requests ([ad10d56](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/ad10d564be02da4b34834628ad44a177ce122b2b))
* **helm:** add extraAnnotations support to ServiceAccount template ([1542192](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/154219294842baff1fe9c3a5c3dc242eaeacd9e4))
* **helm:** add missing RBAC rules and fix port parsing (FB-553) ([9f21c61](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/9f21c6135426fd1e35076b6119bee3cf234e3bc5))
* **helm:** symlink CRDs and derive ports from values (FB-553) ([46a0bdb](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/46a0bdbb9558d494d0e6e62593e961cf421b1ffa))
* improvements to tests ([43cf518](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/43cf518ceac8fc6e667b8a1f8a481b4bcf2f2bdf))
* misc bugs ([9a6f1f3](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/9a6f1f3a8634ebe7f500308c8aed716b08bd475d))
* remove Helm chart & test fixes (FB-660) (#7) ([53f7151](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/53f71511d7034824376197202d84df0763dce75f))
* respect the bind address for http and health probes (FB-553) ([e5fdad5](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e5fdad54c320f645fb0d56e639fdcbe7c96ab2ac))



# 0.1.0

Initial release.
