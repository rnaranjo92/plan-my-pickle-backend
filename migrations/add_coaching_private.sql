-- Instructor Mode: make coaching clips PRIVATE. Student videos shouldn't be
-- world-readable by URL (esp. minors). The backend now hands out short-lived
-- SIGNED download URLs (service key) on read, so playback keeps working while the
-- objects are no longer public. The own-folder INSERT policy is unaffected by the
-- public flag, so client-side upload still works. Existing clips (stored as public
-- URLs) keep playing — the backend parses their path and signs them too.
update storage.buckets set public = false where id = 'coaching-videos';
