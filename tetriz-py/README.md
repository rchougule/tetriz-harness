# tetriz-py

The LLM service for the Tetriz Harness recommendations engine. The Go engine
(`recommendations/`) calls this service over plain HTTP for the behavioral/derived work that
needs a model; everything deterministic stays in Go. The recommendations engine's README
documents the full architecture and examples.

## What it does

Two endpoints back the four carried LLM tasks:

| Endpoint | Tasks |
|---|---|
| `POST /api/v1/generate-content` | `derive_org_artifacts` (DERIVED), `classify_harness_intent` (DETECTED behavioral), `generate_harness_fix` (DETECTED fix) |
| `POST /api/v1/classify-themes` | PR review-theme classification, through **Gemini** (the default; it uses the key the service already needs) or **SBERT** (local, no model key; needs the `ml` extra) |

`GET /_health` (liveness), `GET /readyz` (readiness), and `GET /metrics` (Prometheus) round it out.

## The Gemini key

The provider key is read from the environment at run time and is never stored in a file or
baked into the image:

```bash
export TETRIZ_GEMINI_API_KEY=...   # or the plain GEMINI_API_KEY
```

With no key the service still boots and liveness checks pass, but `/readyz` returns 503;
LLM calls fail until a key is set.

## Run it

The service has no authentication. It binds `127.0.0.1` by default; do not expose it to
other hosts.

From `tetriz-py/`, with [uv](https://docs.astral.sh/uv/):

```bash
uv sync
TETRIZ_GEMINI_API_KEY=... uv run python -m app.main
```

It listens on `http://127.0.0.1:8000`, the URL the recommendations CLI takes as `--tetriz-py-url`.
Review themes are classified through Gemini. For the local SBERT classifier, which needs no
model key, run `uv sync --extra ml` and set `TETRIZ_THEME_CLASSIFIER_STRATEGY=sbert`.

The tests run offline, with the provider mocked:

```bash
uv run pytest
uv run ruff check .
```

## Configuration

Everything is read from `TETRIZ_`-prefixed environment variables (defaults in parentheses):

| Variable | Purpose |
|---|---|
| `TETRIZ_GEMINI_API_KEY` / `GEMINI_API_KEY` | Google Gemini key (required for LLM calls) |
| `TETRIZ_ENVIRONMENT` | `development` / `deploy` / `prod` (`prod`; hides `/docs` outside development) |
| `TETRIZ_HOST` / `TETRIZ_PORT` | listen address (`127.0.0.1` / `8000`) |
| `TETRIZ_GEMINI_MODEL` | default model (`gemini-2.5-flash`) |
| `TETRIZ_THEME_CLASSIFIER_STRATEGY` | default theme strategy, `gemini` / `sbert` (`gemini`; `sbert` needs the `ml` extra) |
| `TETRIZ_THEME_CLASSIFIER_OVERRIDES` | per-org JSON override, e.g. `{"org-x":"gemini"}` |

## The SBERT model

The SBERT strategy embeds review comments with `sentence-transformers/all-MiniLM-L6-v2` from the
Hugging Face Hub (`SBERT_MODEL_NAME` in `app/classifiers/taxonomy.py`; sentence-transformers adds the
`sentence-transformers/` prefix to the bare name).

- **When it is fetched.** The service loads the model at start-up, and only when the `ml` extra is
  installed and the default strategy or a per-org override is `sbert`. On the first load
  sentence-transformers downloads it from `huggingface.co` and caches it, in
  `SENTENCE_TRANSFORMERS_HOME` when set, otherwise in the Hugging Face cache (`HF_HOME`, by default
  `~/.cache/huggingface`). Later starts read the cache.
- **Revision.** No revision is pinned: the first download takes the repository's `main` revision at
  that time, and the cached copy is used after that.
- **Licence.** Apache-2.0, as the model card declares.
- **The image.** The image installs neither the `ml` extra nor the model.

## Licences

tetriz-py is MIT-licensed. `LICENSE` is a copy of the repository's root `LICENSE`, kept here because the
image builds from this directory; `NOTICE` covers the service and its third-party packages.
[`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md) lists every runtime package and every `ml` extra
package with its version and SPDX licence. The image carries all three files in
`/usr/share/licenses/tetriz-py/`.

## Layout

```
app/
├── main.py                       # FastAPI entry; provider + SBERT lifecycle
├── api/                          # health, generate_content, classify_themes
├── llm/
│   ├── router.py                 # TASK_REGISTRY (the 3 generate-content tasks)
│   ├── prompts/ + schemas/       # one file per task
│   ├── providers/                # LLMProvider seam + GeminiProvider (google-genai)
│   └── types.py, exceptions.py
├── classifiers/                  # review-theme: taxonomy, SBERT, Gemini
├── models/                       # Pydantic request/response models
├── config/                       # pydantic-settings (TETRIZ_ env), structlog
└── metrics.py                    # Prometheus collectors (tetriz_py_*)
```

Adding a generate-content task is a `TASK_REGISTRY` entry in `app/llm/router.py` plus a prompt
and schema; the endpoints do not change.
