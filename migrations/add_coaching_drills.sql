-- Instructor Mode: the coach's DRILL LIBRARY. A reusable catalog a coach draws
-- from to build a student's game plan. Two kinds of rows:
--   starter drills — coach_id NULL, is_starter true, seeded below, shared by all
--                    coaches (a useful library on day one).
--   coach drills   — coach_id set, the coach's own custom drills.
-- A coach sees: starter drills OR their own (or=(coach_id.eq.<id>,is_starter.is.true)).
-- Assigning a drill SNAPSHOTS its fields onto coaching_assignments, so editing or
-- deleting a drill never rewrites a student's history. Guarded by columnReady, so
-- deploying before this runs is safe (the Drills tab just stays empty).
create table if not exists coaching_drills (
  id             uuid primary key default gen_random_uuid(),
  coach_id       uuid,                       -- NULL for shared starter drills
  slug           text unique,                -- stable key for idempotent starter seed
  title          text not null,
  skill_category text,                        -- serve|return|dinks|drops|volleys|strategy|footwork
  level_band     text,                        -- e.g. "2.5-3.5", "3.5+"
  format         text,                        -- solo|partner|wall|machine|group
  goal           text,                        -- the concrete target ("25-dink rally, no pop-ups")
  description    text,                        -- how to run it
  video_url      text,
  is_starter     boolean not null default false,
  created_at     timestamptz not null default now()
);
create index if not exists coaching_drills_coach_idx
  on coaching_drills (coach_id, created_at desc);
alter table coaching_drills enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.

-- Starter library (~24 canonical drills). Idempotent via slug conflict.
insert into coaching_drills (slug, title, skill_category, level_band, format, goal, description, is_starter) values
  ('cross-court-dinks', 'Cross-court dink rally', 'dinks', '2.5-3.5', 'partner', 'Sustain a 25-shot rally, zero pop-ups', 'Both players dink cross-court from the NVZ. Keep it soft, in the kitchen, unattackable.', true),
  ('figure-8-dinks', 'Figure-8 dinks', 'dinks', '3.0-4.0', 'partner', 'Directional control both cross-court and down-line', 'Partners dink in a figure-8 pattern (one hits cross-court, the other down-the-line) to train placement + movement.', true),
  ('triangle-dinks', 'Triangle dinks', 'dinks', '3.5+', 'partner', '50-shot rally hitting 3 target spots', 'Feeder dinks to three spots; worker returns each softly. Builds control under movement.', true),
  ('wall-dink-touch', 'Wall dink touch', 'dinks', '2.5-3.5', 'wall', '20 consecutive soft touches', 'Stand a few feet from a wall and dink softly, controlling pace and arc. Great solo touch work.', true),
  ('third-shot-drop-iso', 'Third-shot drop isolation', 'drops', '3.0-4.0', 'partner', '10 in a row landing in the kitchen', 'Feeder returns from the baseline; you hit a soft third-shot drop into the NVZ. Reset and repeat.', true),
  ('drop-and-crash', 'Drop & crash', 'drops', '3.5+', 'partner', '10 rounds: drop then advance to the line', 'Hit a third-shot drop, then move up behind it to the kitchen line. Trains the drop-and-move habit.', true),
  ('transition-zone-drops', 'Transition-zone drops', 'drops', '3.5+', 'partner', '80% soft from mid-court', 'From the transition zone ("no-man''s land"), reset balls softly into the kitchen while moving forward.', true),
  ('deep-drive-targets', 'Deep drive targets', 'drives', '3.0-4.0', 'machine', '75% land deep in-zone over 50 reps', 'Drive returns deep to a target zone near the baseline. Consistency over power.', true),
  ('wall-drives', 'Wall drives', 'drives', '2.5-3.5', 'wall', '30 controlled drives in-zone', 'Drive against a wall to a taped target. Builds a repeatable stroke and pace control.', true),
  ('block-reset-combo', 'Block-reset combo', 'volleys', '3.5+', 'partner', '70% of hard balls land soft in the kitchen', 'Partner drives at you at the line; you block/reset softly rather than counter. Trains hands + calm.', true),
  ('fast-hands-firefight', 'Fast hands / firefight', 'volleys', '3.5+', 'partner', '30s on / 30s off x 5 rounds', 'Both at the line, rapid-fire hands battle. Trains reaction, paddle position, and resets.', true),
  ('reaction-wall-volleys', 'Reaction wall volleys', 'volleys', '3.0-4.0', 'wall', '25 consecutive volleys', 'Volley against a wall without letting the ball bounce. Builds quick hands and a stable paddle.', true),
  ('serve-targets', 'Serve targets', 'serve', '2.5-3.5', 'solo', 'Track % in each deep corner; beat your last %', 'Serve to deep left / deep right target zones. Log makes vs faults to build a reliable serve.', true),
  ('deep-serve-depth', 'Deep serve depth', 'serve', '3.0-4.0', 'solo', '8/10 land in the back third', 'Focus purely on depth — a deep serve buys time and pushes returners back.', true),
  ('return-and-rush', 'Return & rush', 'return', '3.0-4.0', 'partner', '80% deep returns, then reach the line', 'Return deep and immediately move to the kitchen line behind it. The most important habit in doubles.', true),
  ('return-depth-targets', 'Return depth targets', 'return', '2.5-3.5', 'partner', '7/10 land in the back third', 'Practice deep, consistent returns to a target zone. Depth over flash.', true),
  ('shadow-footwork', 'Shadow footwork', 'footwork', '2.5-4.0', 'solo', '3 sets of split-step + move patterns', 'No ball — rehearse split-steps, side-shuffles, and drop-step recovery. Trains movement efficiency.', true),
  ('split-step-timing', 'Split-step timing', 'footwork', '3.0-4.0', 'partner', 'Split on every partner contact for 5 min', 'Partner feeds; you split-step exactly as they contact the ball. Builds ready-position timing.', true),
  ('skinny-singles', 'Skinny singles', 'strategy', '3.0-4.0', 'partner', 'Play to 11 in the diagonal half-court', 'Play singles using only half the court (diagonal). Sharpens consistency, targeting, and shot selection.', true),
  ('speed-up-from-dink', 'Speed-up from the dink', 'strategy', '3.5+', 'partner', 'Convert 5 attackable dinks into won points', 'Dink until you get a ball above net height, then speed it up. Trains recognizing the right ball to attack.', true),
  ('attack-the-backhand', 'Attack the backhand', 'strategy', '3.5+', 'partner', 'Target the backhand 8/10 attacks', 'Deliberately direct speed-ups and drives to the opponent''s backhand — usually the weaker side.', true),
  ('erne-setup', 'Erne setup', 'strategy', '4.0+', 'partner', 'Read 3 dinks wide enough to Erne', 'Practice recognizing a wide dink and jumping around the kitchen for an Erne. Advanced positioning.', true),
  ('reset-from-baseline', 'Reset from the baseline', 'drops', '3.0-4.0', 'partner', '10 soft resets under pressure', 'Partner drives hard; you absorb and reset softly from deep. Builds a calm defensive game.', true),
  ('overhead-put-away', 'Overhead put-away', 'volleys', '3.0-4.0', 'partner', '8/10 overheads placed away from feeder', 'Partner lobs; you put the overhead away to open court. Placement over pure power.', true)
on conflict (slug) do nothing;
