-- Coach intro video: a short "here's my style" clip a coach records to boost
-- conversion. Stores the object PATH in the private coaching-videos bucket (the
-- backend signs it on demand for playback), same as coaching clips.
alter table coach_profiles add column if not exists intro_video_url text;
