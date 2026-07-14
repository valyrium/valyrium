# Changelog

## [1.2.0](https://github.com/valyrium/valyrium/compare/v1.1.0...v1.2.0) (2026-07-14)


### Features

* discover the claude CLI at startup and fail fast if it is missing ([606ca18](https://github.com/valyrium/valyrium/commit/606ca1855c616e73b59c7a3d4d89d5f5e6a882f6))

## [1.1.0](https://github.com/valyrium/valyrium/compare/v1.0.0...v1.1.0) (2026-07-11)


### Features

* accept OpenRouter-style reasoning object and map to --effort ([4acb5d6](https://github.com/valyrium/valyrium/commit/4acb5d6e269ded08f97c141cbe49087e24e91dea))
* add launchd service block to the Homebrew formula ([16c1e31](https://github.com/valyrium/valyrium/commit/16c1e31a9752b1ab2799dd89ffeabe59ffac9fd6))
* cross-request conversation continuity via CLI session resume ([b53235b](https://github.com/valyrium/valyrium/commit/b53235ba2cd416946495d013afb8966ada4672d1)), closes [#13](https://github.com/valyrium/valyrium/issues/13)
* embedded dashboard and persisted token/cost usage ([8ec7316](https://github.com/valyrium/valyrium/commit/8ec7316516fc8187ac5db4e6626f0d29cc0a54c8))
* reap superseded tool-calling sessions immediately ([31cbc5c](https://github.com/valyrium/valyrium/commit/31cbc5c0de3151ac823da2d4edc8a82eabffd8f9)), closes [#14](https://github.com/valyrium/valyrium/issues/14)
* relay thinking blocks as reasoning_content behind a flag ([e2b6c87](https://github.com/valyrium/valyrium/commit/e2b6c874b391a6deb541be113386d0ed75865399))
* report cached tokens via usage.prompt_tokens_details ([38e17ca](https://github.com/valyrium/valyrium/commit/38e17cad322f45ab48b0a248fb81e960550c7c63))
* report context_length/max_model_len in GET /v1/models ([e1877ca](https://github.com/valyrium/valyrium/commit/e1877ca2260c74236b06455786d868144fd253ba))
* support response_format (json_object / json_schema) ([bd02237](https://github.com/valyrium/valyrium/commit/bd02237f541346d59018d398a0da1b461fa52d18))
* tunnel and relay for reaching a local gateway from the internet ([0204823](https://github.com/valyrium/valyrium/commit/0204823093c2420e8c021e61a697e7908eec34a3))


### Bug Fixes

* make completion IDs cryptographically random ([f45c5b9](https://github.com/valyrium/valyrium/commit/f45c5b993eb0613350e2622c77395dbe2c5256d2))
* reap parked sessions on graceful shutdown ([be3a5c5](https://github.com/valyrium/valyrium/commit/be3a5c512f49f85c0929c1c8cc7d609ee6db136e))
* replace bbolt usage store with a zero-dependency JSON ledger ([ff51f64](https://github.com/valyrium/valyrium/commit/ff51f6474453838f36a3d559c004530378a27db6))
* wire finish_reason on non-tool streaming terminal chunk ([69fbdf9](https://github.com/valyrium/valyrium/commit/69fbdf93f314006a16d255bf0383179f88b73167))

## 1.0.0 (2026-07-09)


### Features

* complete MCP relay tool-calling support ([14e96d0](https://github.com/valyrium/valyrium/commit/14e96d0c6fc2d2a43ef45d3cd21910535f2581ba))
* drop TypeScript reference implementation, Go-only from here ([168773c](https://github.com/valyrium/valyrium/commit/168773c57fb3370621f3c9ab57c30a94229036af))
* Go implementation of llm-gateway with core modules and HTTP server ([c74db72](https://github.com/valyrium/valyrium/commit/c74db72fe0e63b3f5b072014d34b07ce3acc9b28))
* WIP MCP relay tool-calling support ([22017b4](https://github.com/valyrium/valyrium/commit/22017b4597299567335a29ce44b9766955b1b6f1))


### Bug Fixes

* strip mcp__relay__ tool-name prefix, log usage on streaming turns ([aea7628](https://github.com/valyrium/valyrium/commit/aea7628d0a628f956896b0d2f3b8b0ed4d65b94b))
