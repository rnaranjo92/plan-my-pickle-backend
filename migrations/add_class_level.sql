-- Skill level for a class (the primary discovery axis after date/location in
-- racket-sport lessons): '', 'beginner', 'intermediate', or 'advanced'.
alter table coaching_classes add column if not exists level text;
