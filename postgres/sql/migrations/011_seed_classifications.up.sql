INSERT INTO classifications (code, name, normal_side, is_system) VALUES
    ('deposit',  'Deposit',  'debit',  true),
    ('withdraw', 'Withdraw', 'credit', true)
ON CONFLICT (code) DO NOTHING;
