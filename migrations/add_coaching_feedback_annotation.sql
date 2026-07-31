-- On-video annotation (telestration): a feedback item can carry a drawing overlay
-- for its pinned moment — {strokes:[{tool,color,points:[[x,y],...]}]} with
-- normalized (0..1) coordinates, re-drawn over the clip when seeking to the moment.
alter table coaching_feedback
  add column if not exists annotation jsonb;
