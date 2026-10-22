CREATE TABLE `users` (
    `id` BIGINT PRIMARY KEY AUTO_INCREMENT,
    `version` INT DEFAULT 0,
    `age` TINYINT NOT NULL,
    `birthdate` DATE,
    `email` VARCHAR(255) NOT NULL,
    `meta` JSON,
    `random_unique` VARCHAR(255) NOT NULL,
    `status` VARCHAR(255),
    `uid` VARCHAR(255) NOT NULL,
    
    `deleted_at` TIMESTAMP NULL,
    
    `created` TIMESTAMP,
    `updated` TIMESTAMP
) ENGINE=InnoDB;

# Unique indexes as they serve Get operations returning single entity
CREATE UNIQUE INDEX `idx_email` ON `users` (`email`);
CREATE UNIQUE INDEX `idx_uid` ON `users` (`uid`);
CREATE UNIQUE INDEX `idx_random_unique` ON `users` (`random_unique`);

# Indexes that serve all operations
CREATE INDEX `idx_age` ON `users` (`age`);
CREATE INDEX `idx_age_created` ON `users` (`age`, `created`);
CREATE INDEX `idx_birthdate` ON `users` (`birthdate`);
CREATE INDEX `idx_status_created` ON `users` (`status`, `created`);



CREATE INDEX `idx_deleted_at` ON `users` (`deleted_at`);
