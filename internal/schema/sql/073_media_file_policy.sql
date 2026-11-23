ALTER TABLE media_objects ADD COLUMN as_file boolean NOT NULL DEFAULT false;
ALTER TABLE media_objects ADD CHECK (NOT as_file OR purpose='message');
