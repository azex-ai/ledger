DROP TRIGGER journal_types_mutation_guard ON journal_types;
CREATE TRIGGER journal_types_mutation_guard
    BEFORE UPDATE ON journal_types
    FOR EACH ROW EXECUTE FUNCTION ledger_block_column_mutation('display_label', 'is_active');

ALTER TABLE journal_types DROP COLUMN holder_kind;
