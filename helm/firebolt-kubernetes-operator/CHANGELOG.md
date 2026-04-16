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
