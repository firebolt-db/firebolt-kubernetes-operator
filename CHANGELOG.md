# Changelog

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
