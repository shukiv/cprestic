-- Restrict the chunker-source check to the moments it means something.
--
-- The original trigger fired on every UPDATE, so marking a server's FIRST
-- repository as provisioned was rejected: by then a second repository
-- existed, and the first one has no chunker source by definition.
--
-- The rule only concerns how a repository is created, so the check belongs
-- on INSERT and on updates that change the two columns it is about.

BEGIN;

DROP TRIGGER repositories_chunker_source ON repositories;

CREATE TRIGGER repositories_chunker_source_insert
    BEFORE INSERT ON repositories
    FOR EACH ROW EXECUTE FUNCTION repositories_check_chunker_source();

CREATE TRIGGER repositories_chunker_source_update
    BEFORE UPDATE OF server_id, chunker_source_repo_id ON repositories
    FOR EACH ROW
    WHEN (OLD.server_id IS DISTINCT FROM NEW.server_id
       OR OLD.chunker_source_repo_id IS DISTINCT FROM NEW.chunker_source_repo_id)
    EXECUTE FUNCTION repositories_check_chunker_source();

COMMIT;
