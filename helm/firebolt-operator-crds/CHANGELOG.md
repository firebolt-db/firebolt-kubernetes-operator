# Changelog

## [0.7.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.6.0...firebolt-operator-crds-chart-0.7.0) (2026-09-01)


### Features

* **api:** add ClusterFireboltEngineClass and namespaced-first engineClassRef (FB-3522) ([#188](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/188)) ([f56fc56](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/f56fc5652ab9f1334df520fc23ed989f1ad4c338))
* verify external postgres server identity ([#183](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/183)) ([#184](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/184)) ([128bfaa](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/128bfaaeb115a303138ac78f8eb823d830ff22c5))

## [0.6.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.5.0...firebolt-operator-crds-chart-0.6.0) (2026-08-29)


### ⚠ BREAKING CHANGES

* **Upgrade to this chart version before you roll out firebolt-operator-chart 0.9.0 / firebolt-operator 0.11.0.** That operator patches existing `FireboltInstance.spec.id` values to lowercase. Against the 0.5.0 CRD the case-only update is rejected at admission, which means **the instance will not start.**
* `FireboltInstance.spec.id` may now change **case only** (`self.lowerAscii() == oldSelf.lowerAscii()`). Every other edit to the field remains forbidden.


### Features

* **api:** mint lowercase FireboltInstance spec.id and allow case-only updates (FB-3516) ([#187](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/187)) ([96310be](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/96310beb7be2952d6b276c2384df1fa5e9188548))

## [0.5.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.4.0...firebolt-operator-crds-chart-0.5.0) (2026-08-25)


### Features

* **api:** add namespaced FireboltEnginePreset CRD (FB-3340) ([#167](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/167)) ([8755d70](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/8755d70bfc2790a548c27b9212df67d983d82bcb))

## [0.4.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.3.1...firebolt-operator-crds-chart-0.4.0) (2026-08-25)


### Features

* **api:** report observed ready replicas on FireboltEngine status (FB-3263) ([#159](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/159)) ([cc89cb1](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/cc89cb127eb500ae2549379926355fd41cdb1a54))

## [0.3.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.3.0...firebolt-operator-crds-chart-0.3.1) (2026-08-17)


### Bug Fixes

* **metrics:** align reconciliation timestamps and documentation (FB-3054) ([c40f3d6](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/c40f3d6248ac537ec60d1266b95a835773064eff))

## [0.3.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.2.2...firebolt-operator-crds-chart-0.3.0) (2026-08-13)


### Features

* add experimental metadata-ng mode (FB-3014) ([7556006](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/755600618ed6d52a850cd19bbc6493f5f1e95cb8))

## [0.2.2](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.2.1...firebolt-operator-crds-chart-0.2.2) (2026-08-12)


### Bug Fixes

* **controller:** surface gateway CRL without client CA when webhooks off (FB-2929) ([#126](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/126)) ([4cb763c](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4cb763c0a719e52280029f0711378424c67b1863))

## [0.2.1](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.2.0...firebolt-operator-crds-chart-0.2.1) (2026-08-03)


### Bug Fixes

* **security:** harden auth & TLS secret, certificate, and admission guards (FB-896) ([#89](https://github.com/firebolt-db/firebolt-kubernetes-operator/issues/89)) ([e7794ee](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/e7794ee489ec149d686c0fbdf768422cf3927e80))

## [0.2.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.1.0...firebolt-operator-crds-chart-0.2.0) (2026-07-27)


### Features

* **api:** add spec.auth and spec.tls to FireboltInstance with validation and RBAC (FB-896) ([59f792b](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/59f792b43c2dd2b59c3be11a9310f392d046dad0))

## [0.1.0](https://github.com/firebolt-db/firebolt-kubernetes-operator/compare/firebolt-operator-crds-chart-0.0.10...firebolt-operator-crds-chart-0.1.0) (2026-06-22)


### ⚠ BREAKING CHANGES

* **controller:** align with engine FHS image layout (FB-1733)

### Features

* **controller:** align with engine FHS image layout (FB-1733) ([4f4a8d4](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/4f4a8d43cdd527976049192fa3a55b2ad6489263))


### Dependencies

* **deps:** set chart appVersion to v0.2.0 ([caf28df](https://github.com/firebolt-db/firebolt-kubernetes-operator/commit/caf28df900a8f729a2b3ab44550a4c980c89ed47))
