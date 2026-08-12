# 0.0.11

appVersion: v0.1.1

## [0.4.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.4.0...firebolt-operator-chart-0.4.1) (2026-08-12)


### Bug Fixes

* **controller:** surface gateway CRL without client CA when webhooks off (FB-2929) ([#126](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/126)) ([4cb763c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4cb763c0a719e52280029f0711378424c67b1863))


### Dependencies

* **deps:** set chart appVersion to v0.6.1 ([d1d8b9f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d1d8b9f08e561d2cb5cfeb8c93154a50312c553a))

## [0.4.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.3.0...firebolt-operator-chart-0.4.0) (2026-08-04)


### Features

* **controller:** wake stopped engines from gateway demand (FB-2553) ([7439de9](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/7439de9f5766f88be58ca2e7bd7cf8bafa39e855))


### Bug Fixes

* **security:** harden auth & TLS secret, certificate, and admission guards (FB-896) ([#89](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/89)) ([e7794ee](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e7794ee489ec149d686c0fbdf768422cf3927e80))


### Dependencies

* **deps:** set chart appVersion to v0.5.0 ([73b1da8](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/73b1da834c2f2f7d1b26b6b5c0d94b422a60f84a))
* **deps:** set chart appVersion to v0.5.1 ([a1674d5](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/a1674d5c87db4ebdcfc7887aa68632065c65fe37))
* **deps:** set chart appVersion to v0.6.0 ([4e4adb7](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4e4adb700185a26a047dfcab1f3d9a8669c0752e))

## [0.3.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.2.0...firebolt-operator-chart-0.3.0) (2026-07-27)


### Features

* **api:** add spec.auth and spec.tls to FireboltInstance with validation and RBAC (FB-896) ([59f792b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/59f792b43c2dd2b59c3be11a9310f392d046dad0))
* **controller:** provision auth, TLS, and coordinated signing-key rotation (FB-896) ([63d000a](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/63d000aced7d64273dab54bd68b2aa36029caa81))


### Dependencies

* **deps:** set chart appVersion to v0.4.1 ([2397638](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2397638b3d526d36744191c05e3d3c5b4dcb774f))

## [0.2.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.1.4...firebolt-operator-chart-0.2.0) (2026-07-21)


### Features

* add anonymous Scarf usage telemetry (FB-1354) ([86ac31b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/86ac31bb83f8c58cc338274149d17ee3a4c3aac8))
* couple telemetry opt-out to default image routing (FB-1354) ([3d78f21](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3d78f218a2cb035917a961e5dd41164c2d5e2ac5))
* route public operator and engine pulls through Scarf (FB-1354) ([3b2a64f](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/3b2a64f50619ae2ed5707737322e16aafc33bee7))


### Dependencies

* **deps:** set chart appVersion to v0.4.0 ([02ea005](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/02ea0058fe94e37358ffc351d7400c330457010c))

## [0.1.4](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.1.3...firebolt-operator-chart-0.1.4) (2026-07-14)


### Dependencies

* **deps:** set chart appVersion to v0.3.2 ([bb74299](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/bb74299501541213d058bd8cd0af4ea859cd6b7e))

## [0.1.3](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.1.2...firebolt-operator-chart-0.1.3) (2026-07-14)


### Dependencies

* **deps:** set chart appVersion to v0.3.1 ([b785a94](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/b785a94a9f61d1eead1956555c69cc8666d2c9c6))

## [0.1.2](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.1.1...firebolt-operator-chart-0.1.2) (2026-07-14)


### Dependencies

* **deps:** set chart appVersion to v0.3.0 ([d347311](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/d34731195eedd0b182f6dfa07daa0ff75e2d8c88))

## [0.1.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.1.0...firebolt-operator-chart-0.1.1) (2026-06-24)


### Dependencies

* **deps:** set chart appVersion to v0.2.1 ([063b578](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/063b578cac21e756ff996790f0aec0fda6ddd8ae))

## [0.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-chart-0.0.10...firebolt-operator-chart-0.1.0) (2026-06-22)


### ⚠ BREAKING CHANGES

* **controller:** align with engine FHS image layout (FB-1733)

### Features

* **controller:** align with engine FHS image layout (FB-1733) ([4f4a8d4](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4f4a8d43cdd527976049192fa3a55b2ad6489263))


### Dependencies

* **deps:** set chart appVersion to v0.2.0 ([caf28df](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/caf28df900a8f729a2b3ab44550a4c980c89ed47))

## [0.1.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v0.1.0...v0.1.1) (2026-06-19)

### Dependencies

* **deps:** bump packdb engine/metadata to 4.32.0-pre.0.20260612115847.e36d5e7953e5 ([2be5de3](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/2be5de362819ce2183c5d39f1a2e6a11d82d60c0))


# 0.0.10

appVersion: v0.1.0

## [0.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/v0.0.9...v0.1.0) (2026-06-12)

### Features

* set new version (FB-1648) ([#9](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/9)) ([9b58ce1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/9b58ce134691e1ade661a7d680dfec5018bad6db))


# Changelog

## 0.1.0

Initial public release.
