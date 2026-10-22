CREATE TABLE `posts` (
    `id` BIGINT PRIMARY KEY AUTO_INCREMENT,
    `version` INT DEFAULT 0,
    `deleted` BOOLEAN NOT NULL,
    `expires_at` DATETIME NOT NULL,
    `language_id` VARCHAR(255) NOT NULL,
    `post` TEXT NOT NULL,
    `revoked` BOOLEAN NOT NULL,
    `story_uid` VARCHAR(255) NOT NULL,
    `target_age` TINYINT NOT NULL,
    `user_id` VARCHAR(255) NOT NULL,
    
    `created` TIMESTAMP,
    `updated` TIMESTAMP
) ENGINE=InnoDB;

# Unique indexes as they serve Get operations returning single entity

# Indexes that serve all operations
CREATE INDEX `idx_deleted_target_age_created` ON `posts` (`deleted`, `target_age`, `created`);
CREATE INDEX `idx_expires_at_revoked_updated` ON `posts` (`expires_at`, `revoked`, `updated`);
CREATE INDEX `idx_language_id` ON `posts` (`language_id`);
CREATE INDEX `idx_user_id_story_uid` ON `posts` (`user_id`, `story_uid`);


