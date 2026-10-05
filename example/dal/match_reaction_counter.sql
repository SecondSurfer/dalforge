CREATE TABLE match_reaction_counters (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    version INT DEFAULT 0,
    match_uid VARCHAR(255) NOT NULL,
    reaction_count INT NOT NULL,
    
    created TIMESTAMP,
    updated TIMESTAMP
) ENGINE=InnoDB;

# Unique indexes as they serve Get operations returning single entity
CREATE UNIQUE INDEX idx_match_uid ON match_reaction_counters (match_uid);

# Indexes that serve all operations


