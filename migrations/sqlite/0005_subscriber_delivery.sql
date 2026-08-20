-- 0005_subscriber_delivery — what a status page has to keep in order to write to
-- its own subscribers.
--
-- Migration 0003 recorded subscriptions and could not deliver to them, and one
-- column is the reason. Every notification a subscriber receives has to carry a
-- one-click unsubscribe link, which means rendering their unsubscribe token
-- again, months after it was issued — and 0003 stores only its hash.
--
-- The rule the data model states is "hash what you verify, encrypt what you
-- replay" (§12.1). This token is both: it is verified when somebody follows the
-- link, and replayed at the foot of every message. So it is stored both ways —
-- the hash carries the unique index the lookup probes, and the envelope carries
-- the value nothing else can reproduce.
--
-- PRE-RELEASE: nothing has shipped, so this file may still change. From Phase 1's
-- first tagged release it is immutable per data model §8.

-- AES-256-GCM envelope, AAD-bound to (org_id, 'subscribers', 'target', id) —
-- the same binding the address beside it uses, so a blob moved onto another
-- subscriber's row fails to open rather than unsubscribing the wrong person.
--
-- Nullable, and rows written before this migration start null. They are not
-- left that way: a subscriber whose token cannot be rendered would receive mail
-- with no way out of it, so delivery issues them a fresh token on first use and
-- rewrites both columns together. The alternative — sending without the link —
-- is how a status page gets reported as spam.
ALTER TABLE subscribers ADD COLUMN unsubscribe_token_encrypted BLOB;

-- The confirmation lookup, which 0003 left to a scan.
--
-- Both token columns are now indexed, which is what makes the resolver's
-- `confirm_token_hash = ? OR unsubscribe_token_hash = ?` two index probes rather
-- than a table scan — SQLite takes the OR apart only when both sides are
-- indexed. It matters because that endpoint is unauthenticated and the token in
-- the path is the whole credential: guessing has to cost a probe against a
-- unique index, not a walk of every subscriber on the instance.
--
-- Partial, because the column is set to NULL the moment a subscription is
-- confirmed and a unique index over many NULLs would otherwise be a unique index
-- over every confirmed subscriber.
CREATE UNIQUE INDEX idx_subscribers_confirm ON subscribers (confirm_token_hash)
    WHERE confirm_token_hash IS NOT NULL;
