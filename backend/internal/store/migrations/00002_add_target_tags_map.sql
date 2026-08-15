-- Add per-target tag mapping (source tag -> target tag value). Stored as a
-- JSON object string; see BIZ-SPEC §6 and adapters.SiteConfig.TagsMap.
ALTER TABLE targets ADD COLUMN tags_map TEXT NOT NULL DEFAULT '{}';
