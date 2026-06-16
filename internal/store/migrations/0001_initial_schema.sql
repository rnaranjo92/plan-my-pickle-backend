-- ============================================================================
-- PlanMyPickle — 0001_initial_schema.sql
-- Initial schema for the pickleball event MVP.
--
-- Target: local SQLite (via Drift) for the MVP.
-- Designed to be SYNC-READY for a future Postgres/Supabase server:
--   * Primary keys are TEXT UUIDs (app-generated), never AUTOINCREMENT.
--   * Every row carries created_at / updated_at; sync-relevant tables carry
--     synced_at (NULL = not yet pushed to a server).
--   * Money is stored as INTEGER cents (never floats).
--   * Timestamps are ISO-8601 UTC TEXT.
--
-- Postgres porting notes are inlined as  -- PG: ...  comments.
-- Run order: this file is migration version 1.
-- ============================================================================

PRAGMA foreign_keys = ON;          -- SQLite has FKs OFF by default; turn them on.

-- ---------------------------------------------------------------------------
-- Migration bookkeeping (framework-agnostic; Drift also tracks user_version).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- ===========================================================================
-- players — a person who can register for events. (#4 registration, #5 SMS)
-- ===========================================================================
CREATE TABLE players (
  id           TEXT PRIMARY KEY NOT NULL,            -- app-generated UUIDv4. PG: uuid DEFAULT gen_random_uuid()
  full_name    TEXT NOT NULL,
  email        TEXT,
  phone        TEXT,                                 -- E.164 ("+15551234567") for SMS (#5)
  skill_level  REAL,                                 -- self-rated (2.5/3.0/...)
  dupr_id      TEXT,                                  -- player's DUPR id; required to submit sanctioned results
  dupr_rating  REAL,                                  -- player's actual DUPR rating (2.0-8.0)
  dupr_reliability REAL,                              -- DUPR reliability/confidence score (0-100)
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at    TEXT
);

-- ===========================================================================
-- events — one event / league / open-play session.
-- ===========================================================================
CREATE TABLE events (
  id                     TEXT PRIMARY KEY NOT NULL,
  name                   TEXT NOT NULL,
  description            TEXT,
  -- play format
  format                 TEXT NOT NULL DEFAULT 'doubles'
                           CHECK (format IN ('singles', 'doubles')),
  -- for doubles: do partners stay fixed or rotate every round? (#3)
  partner_mode           TEXT NOT NULL DEFAULT 'rotating'
                           CHECK (partner_mode IN ('fixed', 'rotating', 'na')),
  -- which leaderboard decides the champion (#2). Both are always computed.
  scoring_mode           TEXT NOT NULL DEFAULT 'wins'
                           CHECK (scoring_mode IN ('points', 'wins')),
  -- competition structure, chosen per tournament at creation.
  tournament_format      TEXT NOT NULL DEFAULT 'round_robin'
                           CHECK (tournament_format IN ('round_robin', 'single_elim', 'pools_playoff')),
  num_courts             INTEGER NOT NULL DEFAULT 1 CHECK (num_courts >= 1),
  points_to_win          INTEGER NOT NULL DEFAULT 11 CHECK (points_to_win >= 1),
  -- tournament sanctioning: DUPR-rated results vs casual local play.
  dupr_sanctioned        INTEGER NOT NULL DEFAULT 0 CHECK (dupr_sanctioned IN (0, 1)),
  dupr_event_id          TEXT,   -- DUPR-side event/league id (partner API), set when sanctioned
  admin_passcode         TEXT,   -- coordinator passcode gating the admin score-entry page (local MVP)
  -- money is in cents; single account collects (no payouts in MVP) (#4)
  registration_fee_cents INTEGER NOT NULL DEFAULT 0 CHECK (registration_fee_cents >= 0),
  currency               TEXT NOT NULL DEFAULT 'USD',
  location               TEXT,
  starts_at              TEXT,
  status                 TEXT NOT NULL DEFAULT 'draft'
                           CHECK (status IN ('draft', 'open', 'in_progress', 'completed', 'cancelled')),
  created_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at              TEXT
);

-- ===========================================================================
-- brackets — skill divisions within a tournament, by DUPR rating range
-- (e.g. "2.5-3.0", "3.0-3.5"). Casual events get a single "Open" bracket.
-- Registrations, rounds and matches each belong to a bracket so every
-- division is scheduled and ranked independently.
-- ===========================================================================
CREATE TABLE brackets (
  id          TEXT PRIMARY KEY NOT NULL,
  event_id    TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  name        TEXT NOT NULL,                          -- "3.0-3.5", "Open"
  min_rating  REAL,                                   -- DUPR lower bound (incl); null = open
  max_rating  REAL,                                   -- DUPR upper bound (incl); null = open
  min_age     INTEGER,                                -- age division lower bound (incl); null = any
  max_age     INTEGER,                                -- age division upper bound (incl); null = any
  sort_order  INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (event_id, name)
);

-- ===========================================================================
-- courts — named courts available for an event (used in SMS "Court 3"). (#3, #5)
-- ===========================================================================
CREATE TABLE courts (
  id           TEXT PRIMARY KEY NOT NULL,
  event_id     TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  label        TEXT NOT NULL,                        -- "Court 1", "Court A"
  court_number INTEGER NOT NULL,                     -- used in match assignment + SMS
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (event_id, court_number)
);

-- ===========================================================================
-- registrations — a player signed up for an event. (#1 check-in, #4 payment)
-- ===========================================================================
CREATE TABLE registrations (
  id              TEXT PRIMARY KEY NOT NULL,
  event_id        TEXT NOT NULL REFERENCES events (id)  ON DELETE CASCADE,
  player_id       TEXT NOT NULL REFERENCES players (id) ON DELETE CASCADE,
  -- fixed-doubles partner (NULL for singles / rotating). Self-referential to players.
  partner_id      TEXT REFERENCES players (id) ON DELETE SET NULL,
  -- skill division the player registered into.
  bracket_id      TEXT REFERENCES brackets (id) ON DELETE SET NULL,
  payment_status  TEXT NOT NULL DEFAULT 'unpaid'
                    CHECK (payment_status IN ('unpaid', 'pending', 'paid', 'refunded', 'comped')),
  -- contactless check-in (#1). Geofence flavor is a later (mobile) add.
  checked_in      INTEGER NOT NULL DEFAULT 0 CHECK (checked_in IN (0, 1)),
  checked_in_at   TEXT,
  check_in_method TEXT CHECK (check_in_method IN ('qr', 'geofence', 'code', 'manual')),
  -- opaque token encoded into the player's check-in QR / code.
  check_in_token  TEXT,
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at       TEXT,
  UNIQUE (event_id, player_id)
);

-- ===========================================================================
-- payments — payment attempts/records for a registration. (#4)
-- Single account collects in MVP; provider rows future-proof Stripe/PayPal.
-- ===========================================================================
CREATE TABLE payments (
  id              TEXT PRIMARY KEY NOT NULL,
  registration_id TEXT NOT NULL REFERENCES registrations (id) ON DELETE CASCADE,
  provider        TEXT NOT NULL
                    CHECK (provider IN ('stripe', 'paypal', 'venmo', 'manual')),
  provider_ref    TEXT,                              -- Stripe session/charge id, etc.
  amount_cents    INTEGER NOT NULL CHECK (amount_cents >= 0),
  currency        TEXT NOT NULL DEFAULT 'USD',
  status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'paid', 'failed', 'refunded')),
  paid_at         TEXT,
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at       TEXT
);

-- ===========================================================================
-- rounds — a round of the generated schedule. (#3, #5 "round starting")
-- ===========================================================================
CREATE TABLE rounds (
  id           TEXT PRIMARY KEY NOT NULL,
  event_id     TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  bracket_id   TEXT REFERENCES brackets (id) ON DELETE CASCADE,
  round_number INTEGER NOT NULL,
  status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'active', 'completed')),
  started_at   TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (event_id, bracket_id, round_number)
);

-- ===========================================================================
-- matches — one game on one court in one round. (#2 scoring, #3 schedule)
-- Sides are "team 1" and "team 2" (1 player each for singles, 2 for doubles).
-- ===========================================================================
CREATE TABLE matches (
  id           TEXT PRIMARY KEY NOT NULL,
  event_id     TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  bracket_id   TEXT REFERENCES brackets (id) ON DELETE CASCADE,
  round_id     TEXT REFERENCES rounds (id) ON DELETE CASCADE,  -- null for playoff (bracket) matches
  court_id     TEXT REFERENCES courts (id) ON DELETE SET NULL,
  -- elimination structure (stage='bracket'); pool matches use round_id.
  stage          TEXT NOT NULL DEFAULT 'pool' CHECK (stage IN ('pool', 'bracket')),
  bracket_round  INTEGER,                            -- 1 = first playoff round; max = FINAL
  bracket_slot   INTEGER,                            -- position within that bracket round
  feeds_match_id TEXT REFERENCES matches (id) ON DELETE SET NULL,  -- winner advances here
  feeds_slot     INTEGER CHECK (feeds_slot IN (1, 2)),            -- into which side (team 1/2)
  team1_score  INTEGER CHECK (team1_score IS NULL OR team1_score >= 0),
  team2_score  INTEGER CHECK (team2_score IS NULL OR team2_score >= 0),
  winning_team INTEGER CHECK (winning_team IN (1, 2)),   -- NULL until scored
  status       TEXT NOT NULL DEFAULT 'scheduled'
                 CHECK (status IN ('scheduled', 'in_progress', 'completed')),
  scheduled_at TEXT,
  completed_at TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at    TEXT
);

-- ===========================================================================
-- match_participants — which players are on which side of a match.
-- One row per player per match. Handles singles (2 rows) and doubles (4 rows)
-- and rotating partners (partners differ per round) uniformly.
-- ===========================================================================
CREATE TABLE match_participants (
  id         TEXT PRIMARY KEY NOT NULL,
  match_id   TEXT NOT NULL REFERENCES matches (id) ON DELETE CASCADE,
  player_id  TEXT NOT NULL REFERENCES players (id) ON DELETE CASCADE,
  team       INTEGER NOT NULL CHECK (team IN (1, 2)),
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (match_id, player_id)
);

-- ===========================================================================
-- notifications — outbound message log (SMS now; push/email later). (#5)
-- ===========================================================================
CREATE TABLE notifications (
  id           TEXT PRIMARY KEY NOT NULL,
  event_id     TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  player_id    TEXT REFERENCES players (id) ON DELETE SET NULL,
  match_id     TEXT REFERENCES matches (id) ON DELETE SET NULL,
  channel      TEXT NOT NULL DEFAULT 'sms'
                 CHECK (channel IN ('sms', 'push', 'email')),
  type         TEXT NOT NULL,                        -- e.g. 'game_starting'
  to_address   TEXT NOT NULL,                        -- phone number for SMS
  body         TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'sent', 'failed')),
  provider_ref TEXT,                                 -- Twilio message SID
  sent_at      TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at    TEXT
);

-- ===========================================================================
-- dupr_submissions — queue/log of completed-match results sent to DUPR for
-- DUPR-SANCTIONED events only (casual events create no rows here).
-- Real submission needs the DUPR partner API + a server; rows queue locally
-- and an "Import to DUPR" action flushes them.
-- ===========================================================================
CREATE TABLE dupr_submissions (
  id           TEXT PRIMARY KEY NOT NULL,
  event_id     TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  match_id     TEXT NOT NULL REFERENCES matches (id) ON DELETE CASCADE,
  status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'submitted', 'failed')),
  provider_ref TEXT,                                 -- DUPR match id once accepted
  error        TEXT,                                 -- failure reason (e.g. missing DUPR id)
  submitted_at TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at    TEXT,
  UNIQUE (match_id)                                  -- one submission per match
);

-- ===========================================================================
-- shirt_orders — optional tournament-shirt customization a player picks after
-- registering (size / name / number / color). One per registration.
-- ===========================================================================
CREATE TABLE shirt_orders (
  id              TEXT PRIMARY KEY NOT NULL,
  registration_id TEXT NOT NULL REFERENCES registrations (id) ON DELETE CASCADE,
  size            TEXT NOT NULL,                       -- XS,S,M,L,XL,XXL,3XL
  name_on_shirt   TEXT,
  number          TEXT,
  color           TEXT,
  status          TEXT NOT NULL DEFAULT 'requested'
                    CHECK (status IN ('requested', 'ordered', 'printed', 'delivered')),
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  synced_at       TEXT,
  UNIQUE (registration_id)
);

-- ---------------------------------------------------------------------------
-- Indexes for the hot read paths.
-- ---------------------------------------------------------------------------
CREATE INDEX idx_courts_event             ON courts (event_id);
CREATE INDEX idx_brackets_event           ON brackets (event_id);
CREATE INDEX idx_registrations_bracket    ON registrations (bracket_id);
CREATE INDEX idx_rounds_bracket           ON rounds (bracket_id);
CREATE INDEX idx_matches_bracket          ON matches (bracket_id);
CREATE INDEX idx_registrations_event      ON registrations (event_id);
CREATE INDEX idx_registrations_player     ON registrations (player_id);
CREATE INDEX idx_payments_registration    ON payments (registration_id);
CREATE INDEX idx_rounds_event             ON rounds (event_id);
CREATE INDEX idx_matches_event            ON matches (event_id);
CREATE INDEX idx_matches_round            ON matches (round_id);
CREATE INDEX idx_matches_court            ON matches (court_id);
CREATE INDEX idx_match_participants_match ON match_participants (match_id);
CREATE INDEX idx_match_participants_player ON match_participants (player_id);
CREATE INDEX idx_notifications_event      ON notifications (event_id);
CREATE INDEX idx_dupr_submissions_event   ON dupr_submissions (event_id);

-- ---------------------------------------------------------------------------
-- updated_at auto-bump triggers.
-- The WHEN guard fires the bump ONLY when the writer did not set updated_at
-- itself, which also makes the trigger safe regardless of recursive_triggers.
-- ---------------------------------------------------------------------------
CREATE TRIGGER trg_players_updated_at
  AFTER UPDATE ON players FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE players SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_events_updated_at
  AFTER UPDATE ON events FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE events SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_courts_updated_at
  AFTER UPDATE ON courts FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE courts SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_brackets_updated_at
  AFTER UPDATE ON brackets FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE brackets SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_registrations_updated_at
  AFTER UPDATE ON registrations FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE registrations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_payments_updated_at
  AFTER UPDATE ON payments FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE payments SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_rounds_updated_at
  AFTER UPDATE ON rounds FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE rounds SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_matches_updated_at
  AFTER UPDATE ON matches FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE matches SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_notifications_updated_at
  AFTER UPDATE ON notifications FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE notifications SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_dupr_submissions_updated_at
  AFTER UPDATE ON dupr_submissions FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE dupr_submissions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

CREATE TRIGGER trg_shirt_orders_updated_at
  AFTER UPDATE ON shirt_orders FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
  BEGIN UPDATE shirt_orders SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id; END;

-- ---------------------------------------------------------------------------
-- player_standings — derived leaderboard, one row per (event, player).
-- Powers BOTH leaderboards in #2:
--   * Points board : ORDER BY points_for DESC, wins DESC, point_diff DESC
--   * Wins board   : ORDER BY wins DESC, losses ASC, points_for DESC, point_diff DESC
--       -> i.e. most wins; tie on wins+losses broken by total accumulated
--          points; still tied -> point differential.  (Your exact rules.)
-- Only completed, scored matches count.
-- ---------------------------------------------------------------------------
CREATE VIEW player_standings AS
SELECT
  m.event_id                                                                      AS event_id,
  mp.player_id                                                                    AS player_id,
  COUNT(*)                                                                        AS games_played,
  SUM(CASE WHEN m.winning_team = mp.team THEN 1 ELSE 0 END)                       AS wins,
  SUM(CASE WHEN m.winning_team <> mp.team THEN 1 ELSE 0 END)                      AS losses,
  SUM(CASE mp.team WHEN 1 THEN m.team1_score ELSE m.team2_score END)              AS points_for,
  SUM(CASE mp.team WHEN 1 THEN m.team2_score ELSE m.team1_score END)              AS points_against,
  SUM(CASE mp.team WHEN 1 THEN m.team1_score ELSE m.team2_score END)
    - SUM(CASE mp.team WHEN 1 THEN m.team2_score ELSE m.team1_score END)          AS point_diff
FROM match_participants mp
JOIN matches m ON m.id = mp.match_id
WHERE m.stage = 'pool' AND m.status = 'completed' AND m.winning_team IS NOT NULL
GROUP BY m.event_id, mp.player_id;

-- Mark this migration applied.
INSERT INTO schema_migrations (version) VALUES (1);
PRAGMA user_version = 1;            -- keeps Drift's migrator in sync with v1.
