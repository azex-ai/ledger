DELETE FROM classifications WHERE code IN ('deposit', 'withdraw') AND is_system = true;
