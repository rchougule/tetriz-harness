# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""SBERT-based zero-shot classifier for PR review comments.

Maps each review comment to one of 13 engineering themes using cosine similarity between
the comment's sentence embedding and a precomputed description embedding per theme. Below
CONFIDENCE_THRESHOLD the comment is labelled "other".

Two guards keep non-code-review comments out of the themes (they are the dominant input
on product/spec repos, where most "review" comments are discussion, process chatter, or
bot boilerplate — not code review):
  - a deterministic NOISE filter drops bot/automation boilerplate and trivial approval/ack
    meta before scoring;
  - REJECT anchors give "other" a positive representation: a comment closer to a product /
    metric / meta / bot prototype than to its best theme is labelled "other" instead of
    being forced into the nearest theme. Without this, argmax always picks a theme and any
    comment sharing surface vocabulary with a theme description is mislabelled.

No training data needed.

This module imports ``sentence_transformers``, which ships in the optional ``ml`` extra
(``uv sync --extra ml``). The base install runs every other endpoint without it.
"""

from __future__ import annotations

import re

import structlog
from sentence_transformers import SentenceTransformer, util

from app.classifiers.taxonomy import OTHER_THEME, SBERT_MODEL_NAME, THEME_DESCRIPTIONS
from app.models.classify_themes import ThemeClassification

logger = structlog.get_logger(__name__)

__all__ = ["OTHER_THEME", "SBERT_MODEL_NAME", "THEME_DESCRIPTIONS", "SBERTThemeClassifier", "is_noise"]

# Threshold below which a comment is labelled "other". Set from empirical probing of
# all-MiniLM-L6-v2 against a small review-comment corpus:
#   - clear on-topic matches: 0.65-0.71
#   - borderline on-topic (short phrasing): 0.38-0.41
#   - gibberish / off-topic: 0.05-0.18
# 0.3 lives in the 0.18-0.38 gap — captures borderline-correct matches while still
# filtering gibberish with comfortable margin.
CONFIDENCE_THRESHOLD = 0.3

# The theme taxonomy (THEME_DESCRIPTIONS / OTHER_THEME / SBERT_MODEL_NAME) lives in
# app.classifiers.taxonomy — a dependency-free module — so the Gemini strategy can share it
# without importing sentence-transformers. Re-exported above for callers of this module.

# REJECT anchors — prototypes of what a comment looks like when it is NOT code review.
# Scored the same way as themes; a comment whose best REJECT similarity is >= its best THEME
# similarity is labelled "other". This gives the negative class a positive representation, so
# an off-topic comment is rejected rather than pulled into the nearest theme by argmax. Keys
# are internal (all map to OTHER); only the descriptions are embedded.
REJECT_ANCHORS: dict[str, str] = {
    "product": (
        "a product, design or requirements discussion about the scope of a feature, "
        "personas, roadmap, prioritization, methodology, or what a spec should cover"
    ),
    "metrics": (
        "a discussion about how the product measures, scores or computes a signal — "
        "cost, tokens, PR complexity, review-loop or merge outcomes, session quality, "
        "sample size or statistical significance"
    ),
    "meta": (
        "a short status or approval note: agreeing, looks good to me, merging, marking "
        "something done, or saying that review comments were left or addressed"
    ),
    "bot": ("an automated review-bot message summarizing suggestions or explaining how to configure the review tool"),
    "general": "a general clarifying question or acknowledgement in a discussion thread",
}

# classify_batch takes `.max()` over the reject-anchor similarities, which is undefined for
# an empty set — enforce the invariant at import, not only in tests.
assert REJECT_ANCHORS, "REJECT_ANCHORS must be non-empty"

# NOISE patterns — deterministic pre-filter for input that carries no code-review signal
# regardless of theme similarity, so it should never reach a theme.
#   _BOT_RE  : signatures of common third-party AI review bots. A bot summary IS the whole
#              comment and opens with the bot's header, so the pattern is anchored to the
#              START (after leading markdown/emoji). It matches the header only when the
#              bot's own boilerplate line follows it — the bare phrase alone is not enough,
#              so a human comment that opens by *referring* to the bot is NOT filtered.
#   _META_RE : trivial approval / status / ack notes. Gated on a length ceiling so it only
#              fires on genuinely short meta, never on a substantive comment that merely
#              opens with "fixed"/"done".
_BOT_RE = re.compile(
    r"^[\W\s]*("
    r"codex review\W+here are some automated review suggestions"  # header + its boilerplate line
    r"|here are some automated review suggestions for this pull request"  # boilerplate line alone
    r"|reviewed commit:\s*`?[0-9a-f]{6,}"  # commit-hash label, not the bare phrase
    r")",
    re.IGNORECASE,
)
_META_RE = re.compile(
    r"^\W*(lgtm|looks good( to me)?|approved?|merg(e|ed|ing)|ship it|done|ack|acked|yep|yes|"
    r"\+1|thanks?|(left|check|added|minor|few)\s+(few\s+)?comments?|check\s+comments?|"
    r"closing|superseded by|fixed( in [0-9a-f]{6,})?|addressed|will (fix|do)|good catch)\W*$",
    re.IGNORECASE,
)
# Max length for a comment to be considered trivial meta. Sized so trivial acks cluster
# under it while substantive feedback that opens with an approval word runs longer.
_META_MAX_CHARS = 60


def is_noise(comment: str) -> bool:
    """Whether a comment is automation boilerplate or trivial approval/ack meta — input that
    should be labelled "other" without scoring against themes."""
    collapsed = re.sub(r"\s+", " ", comment).strip()
    if _BOT_RE.search(collapsed):
        return True
    return len(collapsed) <= _META_MAX_CHARS and bool(_META_RE.match(collapsed))


# A real review comment is often a long, multi-topic document. Embedding the WHOLE comment
# averages those topics into one vector that lands near no single theme, so a multi-topic
# comment scores "other". We instead split a comment into sentence/bullet chunks, classify
# each, and max-pool per theme: a comment is "about testing" if ANY of its sentences is.
# Split on sentence enders and newlines only — NOT on ';', which usually joins related
# clauses of one finding rather than separate topics.
_CHUNK_SPLIT_RE = re.compile(r"(?<=[.!?])\s+|\n+")
_HAS_WORD_RE = re.compile(r"[A-Za-z]{3,}")
# Floor for a chunk worth embedding on its own. Kept low because terse review flags carry the
# strongest signal and are legitimately short; the word-check still rejects pure punctuation.
_MIN_CHUNK_CHARS = 8
# Bound the per-comment encode work so a pathological giant comment can't explode the batch.
_MAX_CHUNKS_PER_COMMENT = 120
# Bound the AGGREGATE chunks per classify_batch call (the whole batch is encoded in ONE
# tensor). Past this budget each remaining comment contributes a single whole-comment
# embedding instead of up to _MAX_CHUNKS_PER_COMMENT, so one encode()'s input stays bounded.
_MAX_TOTAL_CHUNKS_PER_BATCH = 20000


def _split_into_chunks(text: str) -> list[str]:
    """Split a comment into sentence/bullet chunks worth embedding (>= a few real words),
    capped. Falls back to the whole comment when it has no splittable sentence structure."""
    parts = (p.strip() for p in _CHUNK_SPLIT_RE.split(text))
    chunks = [p for p in parts if len(p) >= _MIN_CHUNK_CHARS and _HAS_WORD_RE.search(p)]
    if not chunks:
        stripped = text.strip()
        # No sentence-structured chunk cleared the floor: fall back to the whole comment, but
        # only if it has a Latin-script word. A punctuation/emoji-only or wholly non-Latin body
        # (which this English-centric model cannot theme reliably) collapses to a single "other".
        return [stripped] if stripped and _HAS_WORD_RE.search(stripped) else []
    return chunks[:_MAX_CHUNKS_PER_COMMENT]


class SBERTThemeClassifier:
    """Zero-shot theme classifier using SentenceTransformer embeddings.

    The model is loaded and the theme + reject-anchor descriptions are encoded once at
    construction. `classify_batch` is CPU-bound; call it via `run_in_threadpool` from async
    contexts so the event loop stays responsive.
    """

    def __init__(self, model_name: str = SBERT_MODEL_NAME) -> None:
        logger.info("loading SBERT sentence-transformer model", model=model_name)
        self._model = SentenceTransformer(model_name)
        self._theme_names: list[str] = list(THEME_DESCRIPTIONS.keys())
        self._theme_embeddings = self._model.encode(
            list(THEME_DESCRIPTIONS.values()),
            convert_to_tensor=True,
            show_progress_bar=False,
        )
        self._reject_embeddings = self._model.encode(
            list(REJECT_ANCHORS.values()),
            convert_to_tensor=True,
            show_progress_bar=False,
        )
        logger.info(
            "SBERT model loaded; theme classifier ready",
            model=model_name,
            theme_count=len(self._theme_names),
            reject_anchor_count=len(REJECT_ANCHORS),
        )

    def classify_batch(self, comments: list[str], max_themes: int = 1) -> list[ThemeClassification]:
        """Classify a batch of comments, in input order.

        max_themes=1 (default) returns exactly one result per input — the single best theme
        (or "other"). max_themes>1 returns UP TO max_themes results per input (multiple rows
        sharing the same `index`, descending confidence).

        Each comment is split into chunks and every chunk scored against the themes; a theme's
        confidence for the comment is the MAX over its chunks. A chunk contributes a theme only
        when it clears CONFIDENCE_THRESHOLD and is more theme-like than any REJECT anchor. A
        comment with no qualifying chunk is a single "other". `confidence` is clamped to [0, 1].
        """
        if not comments:
            return []

        # Clamp k to >=1 so the ">=1 result per comment" guarantee holds regardless of the
        # caller: the "other" fallbacks always emit one row, so a k<=0 would otherwise drop
        # ONLY the qualifying-theme comments (asymmetric).
        k = max(1, min(max_themes, len(self._theme_names)))
        n_themes = len(self._theme_names)

        # Split each comment into chunks; noise/ack comments are dropped whole (never chunked)
        # and become a single "other". `spans[i]` is the [start, end) range of comment i's
        # chunks in the flat list encoded in one batch below.
        flat: list[str] = []
        spans: list[tuple[int, int]] = []
        budget_spent = False
        for comment in comments:
            if is_noise(comment):
                chunks: list[str] = []
            elif len(flat) >= _MAX_TOTAL_CHUNKS_PER_BATCH:
                stripped = comment.strip()
                chunks = [stripped] if stripped and _HAS_WORD_RE.search(stripped) else []
                budget_spent = True
            else:
                chunks = _split_into_chunks(comment)
            start = len(flat)
            flat.extend(chunks)
            spans.append((start, len(flat)))
        if budget_spent:
            logger.warning(
                "classify_batch aggregate chunk budget hit; overflow comments scored whole-comment",
                comments=len(comments),
                total_chunks=len(flat),
                budget=_MAX_TOTAL_CHUNKS_PER_BATCH,
            )

        theme_sims = reject_sims = None
        if flat:
            chunk_embeddings = self._model.encode(flat, convert_to_tensor=True, show_progress_bar=False)
            theme_sims = util.cos_sim(chunk_embeddings, self._theme_embeddings)
            reject_sims = util.cos_sim(chunk_embeddings, self._reject_embeddings)

        results: list[ThemeClassification] = []
        for i, (start, end) in enumerate(spans):
            if start == end:  # noise / empty → single "other"
                results.append(ThemeClassification(index=i, theme=OTHER_THEME, confidence=0.0))
                continue

            pooled: dict[int, float] = {}  # theme idx -> best qualifying similarity across chunks
            best_theme_sim = 0.0  # best theme sim seen (the "other" confidence when nothing qualifies)
            for row in range(start, end):
                srow = theme_sims[row].tolist()  # list access is far cheaper than tensor indexing
                reject_max = float(reject_sims[row].max())
                for j in range(n_themes):
                    similarity = srow[j]
                    if similarity != similarity:  # NaN (degenerate embedding) — skip
                        continue
                    if similarity > best_theme_sim:
                        best_theme_sim = similarity
                    # Qualify a theme for this chunk: above the confidence floor AND more
                    # theme-like than the nearest reject anchor. Max-pool across chunks.
                    if (
                        similarity >= CONFIDENCE_THRESHOLD
                        and similarity > reject_max
                        and similarity > pooled.get(j, -1.0)
                    ):
                        pooled[j] = similarity

            if not pooled:
                results.append(
                    ThemeClassification(index=i, theme=OTHER_THEME, confidence=max(0.0, min(1.0, best_theme_sim)))
                )
                continue

            # Comment-level top-N over the max-pooled themes, best-first.
            ranked = sorted(pooled.items(), key=lambda kv: -kv[1])[:k]
            for j, similarity in ranked:
                results.append(
                    ThemeClassification(index=i, theme=self._theme_names[j], confidence=max(0.0, min(1.0, similarity)))
                )
        return results
