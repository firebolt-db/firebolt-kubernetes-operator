# Changelog

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
