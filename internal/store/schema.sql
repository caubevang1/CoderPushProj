-- Catalog schema. Every statement must be safe to run again (IF NOT EXISTS).
-- Migrate splits this file on semicolons, so never put one inside a string or a comment.
-- Keys compared in Go (sku, batch_id) use a binary collation so MySQL agrees with Go
-- on which values are equal. The default collation ignores case.

CREATE TABLE IF NOT EXISTS import_runs (
    id                BIGINT       NOT NULL AUTO_INCREMENT,
    status            VARCHAR(32)  NOT NULL,
    next_cursor       VARCHAR(255) NOT NULL DEFAULT '',
    batches_applied   INT          NOT NULL DEFAULT 0,
    skipped_stale     INT          NOT NULL DEFAULT 0,
    skipped_duplicate INT          NOT NULL DEFAULT 0,
    last_error        TEXT         NULL,
    started_at        DATETIME(6)  NOT NULL,
    updated_at        DATETIME(6)  NOT NULL,
    finished_at       DATETIME(6)  NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS products (
    sku          VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    name         VARCHAR(255) NOT NULL DEFAULT '',
    price        BIGINT       NOT NULL DEFAULT 0,
    currency     CHAR(3)      NOT NULL DEFAULT 'VND',
    status       ENUM('active', 'withdrawn') NOT NULL,
    version      BIGINT       NOT NULL,
    last_run_id  BIGINT       NULL,
    updated_at   DATETIME(6)  NOT NULL,
    PRIMARY KEY (sku),
    KEY idx_products_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS applied_batches (
    run_id      BIGINT       NOT NULL,
    batch_id    VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    item_count  INT          NOT NULL,
    applied_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (run_id, batch_id),
    CONSTRAINT fk_batches_run FOREIGN KEY (run_id) REFERENCES import_runs (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS changes (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    run_id      BIGINT       NOT NULL,
    batch_id    VARCHAR(64)  NOT NULL,
    sku         VARCHAR(64)  NOT NULL,
    kind        ENUM('created', 'updated', 'withdrawn', 'reactivated') NOT NULL,
    old_version BIGINT       NULL,
    new_version BIGINT       NOT NULL,
    old_value   JSON         NULL,
    new_value   JSON         NULL,
    created_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_changes_run (run_id),
    KEY idx_changes_sku (sku),
    CONSTRAINT fk_changes_run FOREIGN KEY (run_id) REFERENCES import_runs (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS problems (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    run_id       BIGINT       NOT NULL,
    batch_id     VARCHAR(64)  NOT NULL,
    sku          VARCHAR(64)  NOT NULL DEFAULT '',
    reason       VARCHAR(500) NOT NULL,
    raw_item     JSON         NULL,
    created_at   DATETIME(6)  NOT NULL,
    resolved_at  DATETIME(6)  NULL,
    PRIMARY KEY (id),
    KEY idx_problems_open (resolved_at),
    CONSTRAINT fk_problems_run FOREIGN KEY (run_id) REFERENCES import_runs (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
