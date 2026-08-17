-- One-off cleanup: collapse duplicate coach_students rows that point at the SAME
-- (coach, student account). These happened before the code de-duped by account
-- id — e.g. the original coach↔player thread + a second row created when the same
-- player later enrolled via a coach-led league under a different email/phone.
--
-- SAFE: for each (coach_id, student_id) it KEEPS the row with the most clips
-- (ties → the oldest), and only DELETES the extra rows that have ZERO clips — so
-- no coaching videos are ever lost. Only considers active rows (not ones the
-- student already left).
--
-- Preview first (see what would be deleted):
WITH ranked AS (
  SELECT cs.id,
         cs.coach_id,
         cs.student_id,
         (SELECT count(*) FROM coaching_videos v WHERE v.coach_student_id = cs.id) AS vids,
         cs.created_at,
         row_number() OVER (
           PARTITION BY cs.coach_id, cs.student_id
           ORDER BY (SELECT count(*) FROM coaching_videos v WHERE v.coach_student_id = cs.id) DESC,
                    cs.created_at ASC
         ) AS rn
  FROM coach_students cs
  WHERE cs.student_id IS NOT NULL
    AND cs.left_at IS NULL
)
SELECT * FROM ranked WHERE rn > 1 AND vids = 0;

-- When the preview looks right, run the delete:
WITH ranked AS (
  SELECT cs.id,
         (SELECT count(*) FROM coaching_videos v WHERE v.coach_student_id = cs.id) AS vids,
         row_number() OVER (
           PARTITION BY cs.coach_id, cs.student_id
           ORDER BY (SELECT count(*) FROM coaching_videos v WHERE v.coach_student_id = cs.id) DESC,
                    cs.created_at ASC
         ) AS rn
  FROM coach_students cs
  WHERE cs.student_id IS NOT NULL
    AND cs.left_at IS NULL
)
DELETE FROM coach_students
WHERE id IN (SELECT id FROM ranked WHERE rn > 1 AND vids = 0);
