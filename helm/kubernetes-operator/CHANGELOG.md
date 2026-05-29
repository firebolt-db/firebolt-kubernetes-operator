# 1.0.25

appVersion: v4.1.1

## [4.1.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v4.1.0...v4.1.1) (2026-05-29)

### Bug Fixes

* **api:** preserve template.metadata on embedded PodTemplateSpec fields (FB-556) ([a933314](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a9333144c839055454761a0e47d8f7f8ab22735b))


# 1.0.24

appVersion: v4.1.0

## [4.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v4.0.0...v4.1.0) (2026-05-28)

### Features

* **api:** bound FireboltEngine.spec.resources at admission (FB-556) ([c421174](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c421174dd58d9ca5f60a3b5ce83e1a5f42313fef))
* **controller:** add per-engine Envoy circuit breakers on DFP cluster (FB-556) ([3ac0aa5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3ac0aa550870b3fb60439b1ccf1311f98a384228))
* **controller:** surface external finalizers on engine-owned children (FB-556) ([177f274](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/177f2741708f7693f21420495ebd8574683f3a87))

### Bug Fixes

* **controller:** sync ResourceVersion in updateStatus conflict path (FB-556) ([6227d50](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6227d501c3f426e74dd2282c2a4f26043fb6d486))


# 1.0.23

appVersion: v4.0.0

## [4.0.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v3.1.1...v4.0.0) (2026-05-28)

### ⚠ BREAKING CHANGES

* **api:** drop fictional MetadataSpec.MetricsPort (FB-1322)
* **api:** replace ComponentSpec with PodTemplateSpec on FireboltInstance gateway/metadata (FB-1322)

### Features

* **api:** replace ComponentSpec with PodTemplateSpec on FireboltInstance gateway/metadata (FB-1322) ([dc750b9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/dc750b97083404a3a8c4fe6047658da994bb48a7))

### Bug Fixes

* **api:** drop fictional MetadataSpec.MetricsPort (FB-1322) ([c0b5f11](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c0b5f11dbee44046d3a253ddfb73ce0c2c359458))
* **controller:** skip operator-managed gateway RBAC when user supplies serviceAccountName (FB-1322) ([b729b9e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b729b9e8dcbed9e24e6f2060dbcb3a4b0fd04e93))


# 1.0.22

appVersion: v3.1.1

## [3.1.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v3.1.0...v3.1.1) (2026-05-27)

### Bug Fixes

* **controller:** make engine drain budget monotonic in grace period (FB-1327) ([#77](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/77)) ([6c42fc5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6c42fc577a5713a4a2dedf86dcfd58a0cfbbcb3e))


# 1.0.21

appVersion: v3.1.0

## [3.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v3.0.0...v3.1.0) (2026-05-27)

### Features

* **controller:** default engine container securityContext to hardened non-root (FB-1297) ([fe6f5ce](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/fe6f5ce22fa26b92a3d608f4107487bf7fc12ef2))


# 1.0.20

appVersion: v3.0.0

## [3.0.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.12.0...v3.0.0) (2026-05-26)

### ⚠ BREAKING CHANGES

* **api:** scope EngineClass to namespaces (FB-1145)
* **api:** FireboltEngineSpec.Image is removed. The engine container
image now flows from the operator's embedded default until EngineClass-based
merging lands (the new spec.engineClassRef has no behavioural effect yet).
Migration: update Helm values to set the desired default image cluster-wide,
or wait for the merge layer commit and reference an EngineClass whose
template.containers[engine].image carries the per-class image. Existing
engines with spec.image will fail admission; remove the field or update the
operator's default. The intent is image governance through EngineClass rather
than per-engine duplication; see the e2e Image Switching block (XDescribe'd
here) for the rewrite landing in a follow-up commit.

Tests using the engine container image as a per-generation spec carrier now
use spec.serviceAccountName instead — same stsMatchesSpec drift semantics,
different field.

### Features

* **api:** add EngineClass + FireboltEngine validating webhooks (FB-1145) ([eb10df6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/eb10df68aa40c13d8a970bafad5ad0c4046929c8))
* **api:** introduce EngineClass and route engine image through it (FB-1145) ([1693660](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/1693660d971d3f40402a6a49b5f6a266d66b12cb))
* **controller:** add EngineClass status reconciler (FB-1145) ([6f5bd8b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6f5bd8b037d4b2498b3879d1ab1b50609ac08789))
* **controller:** merge EngineClass template into engine pod spec (FB-1145) ([57b86c1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/57b86c1c3d904e4c3ff42caa287aa9c038ab7181))

### Bug Fixes

* **controller:** apply CI EngineClass to engine namespace; log missing-class refs (FB-1145) ([d7fe90e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d7fe90ef8e55c23a99542e5ad17d5e93abe9fe3c))

### Code Refactoring

* **api:** scope EngineClass to namespaces (FB-1145) ([72ec13d](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/72ec13dd2ed97d229f39819e069321538a5edc33))


# 1.0.19

appVersion: v2.12.0

## [2.12.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.11.0...v2.12.0) (2026-05-26)

### Features

* add log config options and use JSON by default (FB-1222) ([#74](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/74)) ([4a7b30f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4a7b30f254ffeaf6e4cdc3eca812b123e1b3f692))


# 1.0.18

appVersion: v2.11.0

## [2.11.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.10.0...v2.11.0) (2026-05-19)

### Features

* add support for init-containers on engine pods (FB-1234) ([#72](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/72)) ([b39c819](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b39c8190689b071f841e7fb8b4090ad01664a98d))


# 1.0.17

appVersion: v2.10.0

## [2.10.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.9.0...v2.10.0) (2026-05-19)

### Features

* **api:** add FireboltInstance.spec.metricScrapeMode (FB-1085) ([fb8abc5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/fb8abc593906b6be907facc0abf694f7bcf3f609))
* **controller:** pluggable pod-metric scraper for drain probe and autoscaler (FB-1085) ([37bfd2c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/37bfd2c99d575741388ac7ae7d258b4c71abb669))

### Bug Fixes

* **controller:** typed nil clientset in apiserverProxyScraper (FB-1085) ([9cff4e9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9cff4e9e0136ef735041b774d7c48c2b89cb6279))


# 1.0.16

appVersion: v2.9.0

## [2.9.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.8.1...v2.9.0) (2026-05-18)

### Features

* **controller:** surface StatefulSet warning events on engine Ready condition (FB-872) ([#68](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/68)) ([2140525](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2140525f9766f4156cfca852ffe662403dd640ed)), closes [#2](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/2)


# 1.0.15

appVersion: v2.8.1

## [2.8.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.8.0...v2.8.1) (2026-05-18)

### Bug Fixes

* **ci:** disable serviceLinks on floci pod to avoid FLOCI_PORT collision (FB-1215) ([9c16bc9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9c16bc9159f24245cc90476044896726cca12d0a))
* **controller:** disable service-link env injection on operator-managed pods (FB-1215) ([fc07f55](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/fc07f55b24ae74a1e6865656542ac1212aa0bd8f))
* **controller:** harden metadata pod security context (FB-1213) ([960ea08](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/960ea08ee3d9bbe78e1c5f5d31665b520a58366d))
* explicitly use latest versions (FB-1213) ([8860bcb](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8860bcbdf88b89d69544f8529a5e4052e5c0bf09))
* remove unused constants (FB-1213) ([45d094c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/45d094c46bf7b2f8acf1904abdc15752c31fb506))


# 1.0.14

appVersion: v2.8.0

## [2.8.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.7.1...v2.8.0) (2026-05-18)

### Features

* **api:** allow overriding image repository or tag independently (FB-1090) ([3d453d6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3d453d618fe4c02d39a63d9e1b18a7906604e63e))


# 1.0.13

appVersion: v2.7.1

## [2.7.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.7.0...v2.7.1) (2026-05-16)

### Bug Fixes

* **controller:** harden internal PostgreSQL pod security context (FB-1164) ([#62](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/62)) ([1a5e5d1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/1a5e5d1f202c35ca2f4e6cb11f71af9a25850c9d))


# 1.0.12

appVersion: v2.7.0

## [2.7.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.6.0...v2.7.0) (2026-05-15)

### Features

* add support for pod annotations (FB-1148) ([#58](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/58)) ([18bad6c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/18bad6c9fe3a6d872803dfb44ea33a2d0f996c0d))


# 1.0.11

appVersion: v2.6.0

## [2.6.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.5.0...v2.6.0) (2026-05-14)

### Features

* **api:** add pod labels support to FireboltEngineSpec (FB-1149) ([#52](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/52)) ([5172ca2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/5172ca27ca971f90a5de3f3cea86100c801425ed))


# 1.0.10

appVersion: v2.5.0

## [2.5.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.4.1...v2.5.0) (2026-05-14)

### Features

* support node and pod affinity (FB-905) ([#51](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/51)) ([2461722](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2461722cc26934d5bdf5cca10c1e1e467e651e6e))


# 1.0.9

appVersion: v2.4.1

## [2.4.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.4.0...v2.4.1) (2026-05-14)

### Bug Fixes

* xml injection bug (FB-1163) ([#53](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/53)) ([592210c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/592210c042c283cf6052afd95d967e58cdc4eb89))


# 1.0.8

appVersion: v2.4.0

## [2.4.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.3.0...v2.4.0) (2026-05-13)

### Features

* **controller:** render engine config.yaml in new structured schema (FB-985) ([183dad3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/183dad3bebfa5a061a1a9e5522e407e759e781a5))


# 1.0.7

appVersion: v2.3.0

## [2.3.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.2.0...v2.3.0) (2026-05-13)

### Features

* add debug-e2e skill and adjust CI for consistency (FB-1141) ([#50](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/50)) ([6e1c6fe](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6e1c6fe01d332e13b583d3e20dabeed4f32f117d))


# 1.0.6

appVersion: v2.2.0

## [2.2.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.1.0...v2.2.0) (2026-05-13)

### Features

* **api:** add EngineEmptyDirSpec sibling backend on EngineStorageSpec (FB-1085) ([9b705fa](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9b705fa71328e9b83a6426a16757e4fbe9b22ba4))
* **api:** add EngineHostPathSpec sibling backend on EngineStorageSpec (FB-1085) ([51b76c0](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/51b76c0d2d6dc74579465091cd3268e547f589e5))
* **api:** enforce backend mutual-exclusion via CEL on EngineStorageSpec (FB-1085) ([91a29f2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/91a29f254fedd7ebba7d0ec19878c4e5e5464926))
* **controller:** default engine storage to emptyDir when no backend specified (FB-1085) ([9ce972c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9ce972c199877f42eb97090788c62db7177bbad8))
* **controller:** wire EmptyDir backend through buildStatefulSet and storageMatchesSpec (FB-1085) ([d93ffe5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d93ffe598d2b91b88c7f8d8e2b5ea411ff07527f))
* **controller:** wire HostPath backend through buildStatefulSet and storageMatchesSpec (FB-1085) ([ec9694a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/ec9694a1a635789558bc30b0f79f5f89075e6aba))

### Bug Fixes

* **examples:** migrate engine-full manifest and verify script to nested PVC schema (FB-1085) ([4ae0a0e](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4ae0a0e96f45ba6f6b03094aea367fd5b419add8))


# 1.0.5

appVersion: v2.1.0

## [2.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.0.3...v2.1.0) (2026-05-13)

### Features

* **api:** make postgres schema configurable (FB-1088) ([5fe61af](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/5fe61afda1bd2eed28557beef134cf17436dc9f2))

### Bug Fixes

* **controller:** invoke engine via 'firebolt server' with FIREBOLT_CORE_MODE (FB-1088) ([a0ed274](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a0ed274e44cb40735d274c189a278ab8f5d8c9a8))
* **controller:** rename engine binary path to /firebolt-core/firebolt (FB-1088) ([db0a632](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/db0a632dc4c9e2769d87c327b1f29d922ffbb65e))


# 1.0.4

appVersion: v2.0.3

## [2.0.3](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.0.2...v2.0.3) (2026-05-08)

### Bug Fixes

* **ci:** source defaults.<variant>.env in verify-quickstart-full (FB-983) ([aab4f34](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/aab4f34e53e737c3597416f4f504d1a3194d2a4e))
* **helm:** preserve operator's intrinsic namespace label on scrape (FB-840) ([e70dcbd](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e70dcbd3d796c0659764f8f73ff6152702327d48))


# 1.0.3

appVersion: v2.0.2

## [2.0.2](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.0.1...v2.0.2) (2026-05-07)

### Bug Fixes

* **helm:** missing RBAC in ClusterRole (FB-995) ([#39](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/39)) ([e09fddd](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e09fddd5a605e7d0c927ff285191b047c6c748b2))


# 1.0.2

appVersion: v2.0.1

## [2.0.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v2.0.0...v2.0.1) (2026-05-07)

### Bug Fixes

* **metrics:** record CR metrics on every reconcile path ([8fea266](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8fea2665cdd27b843b589f29f93dad592a2d5885))


# 1.0.1

appVersion: v2.0.0

## [2.0.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.25.1...v2.0.0) (2026-05-07)

### ⚠ BREAKING CHANGES

* **helm:** (chart): the `podMonitor.operator.{enabled,interval}`
values are removed and replaced by `serviceMonitor.operator.*`. Default
remains disabled.

### Features

* split defaults.env into latest/dev image variants (FB-983) ([27573ad](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/27573ad8953c387ca80259ddec119e7940587063))

### Bug Fixes

* **helm:** scrape operator metrics via ServiceMonitor instead of PodMonitor (FB-840) ([f8c117b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f8c117b0f2386ea9fc32619a0c9c2c388dc5783f))


# 1.0.0

fix(helm): scrape operator metrics via ServiceMonitor instead of PodMonitor (FB-840)

# 0.1.35

appVersion: v1.25.1

## [1.25.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.25.0...v1.25.1) (2026-05-06)

### Bug Fixes

* **gateway:** hard-code envoy per-connection buffer at 2 MiB (FB-849) ([aa37a36](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/aa37a36aca396a1a6ef1f19508d2da9f99dcda5d))


# 0.1.34

appVersion: v1.25.0

## [1.25.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.24.0...v1.25.0) (2026-05-06)

### Features

* **gateway:** retry shutdown-fence 503s via X-Firebolt-Drained (FB-849) ([601ba41](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/601ba41c5c909ddeaf1360d841b21f850b070b87))
* switch to AGENTS.md (FB-849) ([3862e7a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3862e7abe14c819140461a397722dc22c7e9852c))


# 0.1.33

appVersion: v1.24.0

## [1.24.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.23.0...v1.24.0) (2026-05-06)

### Features

* allow AWS SDK EC2 metadata detection via IRSA (FB-875) ([52f154c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/52f154cd6a535b9f9203e3af3c8ad9e7d0893f07))

### Bug Fixes

* always check env variables when comparing spec (FB-875) ([4d41a91](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4d41a91871bd4b5344a7f9eb7760a6b8066e0fcd))


# 0.1.32

appVersion: v1.23.0

## [1.23.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.22.0...v1.23.0) (2026-05-05)

### Features

* **controller:** apply independent engine resource requirements (FB-864) ([#30](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/30)) ([a6d9bd3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a6d9bd3a7ff097fb6b0b3e0006ceeaf5ba023343))


# 0.1.31

appVersion: v1.22.0

## [1.22.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.21.0...v1.22.0) (2026-05-05)

### Features

* bump engine/metadata images and use latest/dev where makes sense (FB-908) ([d6eb7cc](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d6eb7cc460dc3a2fe60066bd2bbafe93f9a30ef9))


# 0.1.30

appVersion: v1.21.0

## [1.21.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.20.0...v1.21.0) (2026-05-05)

### Features

* **autoscaler:** add per-engine idle/schedule autoscaling (FB-903) ([6e28d9b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6e28d9b38288853e789ab322e9a095d32aa8caec))
* **autoscaler:** wake-up annotation contract + per-instance gateway RBAC (FB-903) ([b860ac3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b860ac3fa1f7322c2021103f4441393718fef591))

### Bug Fixes

* **autoscaler:** reject minReplicas > maxReplicas at admission (FB-908) ([11ab027](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/11ab0272d8a1a91287da3df60cec6aab11181594))
* disallow user overrides for config.multi_engine_mode_enabled (FB-908) ([f8a0c1a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f8a0c1aff9cf255ee7f842f91cdd704c42fdda90))


# 0.1.29

appVersion: v1.20.0

## [1.20.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.19.0...v1.20.0) (2026-04-30)

### Features

* **controller:** add TLA+ state-cover tests for engine reconciler (FB-804) ([13b6087](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/13b608722b921f90eca8a929671ff6d4c1824192))

### Bug Fixes

* **controller:** order TLA+ state-cover fixture by content (FB-804) ([c7343ab](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c7343abb8ba98887ff39695bda1dfaf4128f140c))


# 0.1.28

appVersion: v1.19.0

## [1.19.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.18.0...v1.19.0) (2026-04-29)

### Features

* include more log lines to troubleshoot issues (FB-903) ([6c34153](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6c34153583946bc86d8c28b428d95ce2daf834c4))

### Bug Fixes

* Core -> Engine (FB-903) ([9ab7f45](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9ab7f45b7740e083a0131ef6509e33cbc12462a3))
* no need to wait for Postgres before starting metadata service (FB-903) ([47e5c93](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/47e5c934a05151a853567968e2ab452595404831))
* rename pensieve -> metadata (FB-903) ([76ce71a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/76ce71ae89ef118da1eed3ec33f02d0991f117ce))


# 0.1.27

appVersion: v1.18.0

## [1.18.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.17.1...v1.18.0) (2026-04-29)

### Features

* push also latest tag for operator image (FB-881) ([be71336](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/be713363c830e406ec0ebf73f0559feeaa761155))

### Bug Fixes

* override custom engine config at root (FB-902) ([a0d5d1c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a0d5d1cc4cbe324b78f951b5487b770a581af2bf))


# 0.1.26

appVersion: v1.17.1

## [1.17.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.17.0...v1.17.1) (2026-04-29)

### Bug Fixes

* mention difference in semver tags for charts (FB-881) ([b775e98](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b775e983488177f679d29c7f8d61d1126751a560))
* use latest engine/metadata images (FB-881) ([66c0f43](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/66c0f4394ff424841bc5843b42396821d648c3d2))


# 0.1.25

appVersion: v1.17.0

## [1.17.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.16.0...v1.17.0) (2026-04-29)

### Features

* always generate JSON schema (FB-879) ([5923ab2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/5923ab289b98558bfecfcedd049fbd7c480092fb))


# 0.1.24

appVersion: v1.16.0

## [1.16.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.15.0...v1.16.0) (2026-04-29)

### Features

* **engine:** add securityContext spec fields with default fsGroup 3473 (FB-873) ([ffa319b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/ffa319ba3db467c270240cb2e95bd254c7fbcc2e))


# 0.1.23

appVersion: v1.15.0

## [1.15.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.14.0...v1.15.0) (2026-04-28)

### Features

* reference KSA per engine (FB-870) ([#18](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/18)) ([e6d6aeb](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e6d6aeb0be1d770c738f8592ce371366897f7327))


# 0.1.22

appVersion: v1.14.0

## [1.14.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.13.1...v1.14.0) (2026-04-28)

### Features

* **engine:** add customEngineConfig spec field (FB-866) ([f4f60e1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f4f60e12d5f38129e08047614d336511f9c62a7e))

### Bug Fixes

* **ci:** defer helm-release-cd correctly on rebase merges (FB-810) ([7a06f8a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7a06f8a175f11ffa8d155031ef9fa865ac371ec2))


# 0.1.21

appVersion: v1.13.1

## [1.13.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.13.0...v1.13.1) (2026-04-28)

### Bug Fixes

* use most recent image (FB-758) ([a976172](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a9761721a0a2cb9d8613d06b3c2753a5f9296181))


# 0.1.20

appVersion: v1.13.0

## [1.13.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.12.0...v1.13.0) (2026-04-28)

### Features

* add embedded Prometheus metrics for CR status (FB-560) ([7aae8e6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7aae8e609c7f0f933aae1774cc9d9e6173b4395b))
* wire CR status metrics into controllers (FB-560) ([f8bb91f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f8bb91ff9b913961d0cb264fb91cfe3c21ff6bdd))

### Bug Fixes

* **ci:** reduce e2e parallelism to 6 to fix memory exhaustion (FB-560) ([6bff89b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/6bff89be2b5ca530aa95f2cdd4772007b165bcb2))


# 0.1.19

fix(ci): reduce e2e parallelism to 6 to fix memory exhaustion (FB-560)

# 0.1.18

appVersion: v1.12.0

## [1.12.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.11.1...v1.12.0) (2026-04-28)

### Features

* expose metrics ports on gateway and metadata pods (FB-839) ([d684d24](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d684d2483f769b503e89f9762f8cdf7baf0686d5))
* **helm:** add Prometheus PodMonitor templates (FB-839) ([7ab2cd0](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7ab2cd02dbd930ddf73d6046eea76d3248eec9a9))

### Bug Fixes

* **ci:** add actions:write permission to app token for workflow dispatch (FB-810) ([f700065](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f700065d740080f08eeff82ca8d80244a0568637))
* trigger new CD (FB-828) ([e5f22a4](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e5f22a424132788104217ea350a1b79e2cebbf0b))


# 0.1.17

fix(ci): add actions:write permission to app token for workflow dispatch (FB-810)

# 0.1.16

appVersion: v1.11.1

## [1.11.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.11.0...v1.11.1) (2026-04-28)

### Bug Fixes

* disallow 2 running metadata services at the same time (FB-828) ([#8](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/8)) ([481e38d](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/481e38da2c7a767603bc3010b7775e1bdfa904f4))


# 0.1.15

appVersion: v1.11.0

## [1.11.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v1.10.2...v1.11.0) (2026-04-27)

### Features

* **engine:** add per-pod PVC at /firebolt-core/volume (FB-820) ([#7](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/7)) ([57f282c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/57f282c36d624cc959fd36215b54e67b145c6b86))
* update examples and use new images (FB-804) ([#6](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/6)) ([c6be6a2](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c6be6a25ea70b4a041087fca600011c53fbc0a51))

### Bug Fixes

* **ci:** disable @semantic-release/github PR/issue comments and grant matching App perms (FB-810) ([#10](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/10)) ([a7c3866](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a7c38661a6a9dc868eb5f219c59c913bbda3552a))
* **ci:** upgrade semantic-release to v25 and switch to GITHUB_TOKEN env (FB-810) ([c83454c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c83454cb467a56c50e636243c8fd4c5fb8f794b0))


# 0.1.14

appVersion: v1.10.2

### [1.10.2](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.10.1...v1.10.2) (2026-04-24)


### Maintenance

* **deps:** bump go.opentelemetry.io/otel/sdk ([7f8d3c7](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7f8d3c78c9a2219c115d8fe8fe5e5325d4c446d7))
* **helm:** bump chart to 0.1.13 (appVersion v1.10.1) ([c3ce441](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/c3ce4414200cd7028bff0da9bc0c0ce2f8e92d21))



# 0.1.13

appVersion: v1.10.1

### [1.10.1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.10.0...v1.10.1) (2026-04-23)


### Maintenance

* **ci:** normalise workflow file extensions to .yaml (FB-769) ([f7f1dd9](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/f7f1dd9fdd86c87211613ca2e3d22d149514364d))
* forbid git add -A in commit workflow (FB-769) ([add7c7a](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/add7c7aa4b85c36e836e35a372ef98fa05fa2d89))
* **helm:** bump chart to 0.1.12 (appVersion v1.10.0) ([66cfa6b](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/66cfa6be5017f7b85a1fb5b5fdd936087f222399))


### Bug fixes

* **engine:** log instance gate rejection reason to operator logs (FB-769) ([8d9ec48](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/8d9ec482d75605350583ff0854b3d1b4b74ca8db))
* **engine:** use foreground propagation for StatefulSet deletion (FB-769) ([4507ab1](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/4507ab140950a77dc2e958a0fb24d0ad47eea624))
* **formal:** tighten ReadyIsStable consequent to []AllReady (FB-769) ([7edac69](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7edac694358ba54b7e77b1f2725b315c70763b59))
* **formal:** use SF_vars for ReconcileRun in FireboltInstance (FB-769) ([54504c3](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/54504c3e0823a7c32f43a730406c15d4a539dc9c))
* **formal:** weaken EventuallyReady to <>[]AllReady precondition (FB-769) ([483858d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/483858df6af2c28c136ae780ec60cef7c6028f10))



# 0.1.12

appVersion: v1.10.0

## [1.10.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.9.0...v1.10.0) (2026-04-23)


### Features

* **engine:** enable drain ejection now that engine serves /health/ready on port 3473 (FB-769) ([2cf62da](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2cf62daa5792d0b0f677d1dd064c3c20abf27286))
* **engine:** remove preStop hook; rely on shutdown_wait_unfinished + Envoy ejection (FB-769) ([47ad26d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/47ad26dadff0cdbeadcf0a2649c6fc663d958806))
* **engine:** set shutdown_wait_unfinished from terminationGracePeriodSeconds (FB-769) ([3e8cafe](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/3e8cafefb69cf6c16ea13ecf94010e4d76de4067))
* **instance:** remove gRPC account init — Pensieve Dedicated handles account creation (FB-769) ([6f4b340](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/6f4b34023a8bc41f0123966cc38a30191148fb7c))
* use most recent image with all planned fixes (FB-769) ([e10fa8f](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e10fa8f213b263b7c06020d5896ca3a7c6e001be))


### Maintenance

* **helm:** bump chart to 0.1.11 (appVersion v1.9.0) ([26c870d](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/26c870d0da1664c70c75385802c4b693ac48d865))


### Bug fixes

* **e2e:** verify health check ejection via failure counter delta (FB-769) ([e95d254](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e95d254bbb7e9b758cdebba7b05948ff8f00c007))
* **engine:** correct shutdown_wait_unfinished to PreStopGraceMarginSeconds (FB-769) ([859c751](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/859c75184d083fdfc30d371b10b31a0f20a5a526))
* **engine:** match labeled metrics in preStop drain check (FB-769) ([a2c1c99](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/a2c1c99d997f230810ce91e9a7a8b950247317c7))
* **engine:** use integer seconds for shutdown_wait_unfinished (FB-769) ([42f2f43](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/42f2f43db0caa29d164b5379e122b81a648bdfd0))
* **gateway:** repair active health checks and add E2E verification (FB-760) ([7e70d98](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/7e70d9818810a0542e8f63a45f29d913c8c07079))



# 0.1.11

appVersion: v1.9.0

## [1.9.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.8.0...v1.9.0) (2026-04-22)


### Features

* **helm:** add extraVolumes and extraVolumeMounts support (FB-553) ([1720230](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/172023086cf854c15d77c7637e38e9c8c976611b))
* **helm:** expose named container ports for health, metrics, webhook (FB-553) ([1415b30](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1415b303e044475075b118a3f933d5dd8ae71ac3))
* **helm:** extend values.schema.json for webhook and volume values (FB-553) ([e00dc42](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/e00dc429222af9a351179f5a4d340cfc442f1134))
* **helm:** render Mutating/ValidatingWebhookConfigurations for FireboltInstance (FB-553) ([0dc86f2](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/0dc86f276bc3e70fc844924dc6809331e7bc57f3))
* **helm:** wire admission webhook plumbing behind a toggle (FB-553) ([2009649](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2009649db1ccbf3a156a07f52c4433abcd429683))


### Bug fixes

* **helm:** make podSecurityContext configurable with fsGroup default (FB-553) ([1fd3e98](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1fd3e98d0f4325d4bbb90e329c55b2b7947ca7c8))



# 0.1.10

appVersion: v1.7.0

## [1.7.0](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/compare/v1.6.0...v1.7.0) (2026-04-21)


### Features

* **test:** add rapid stateful property tests for engine reconciler (FB-700) ([1437bc5](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/1437bc5657a338a3717cdfb833a9c7848611aff8))


### Maintenance

* **helm:** bump chart to 0.1.9 (appVersion v1.6.0) ([2f3b54f](https://github.com/firebolt-analytics/firebolt-kubernetes-operator/commit/2f3b54f30f839ff4a54a633b36e133ad9ba0621e))



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
