CREATE TABLE currencies (
    id     BIGSERIAL PRIMARY KEY,
    code   TEXT UNIQUE NOT NULL,
    name   TEXT NOT NULL
);
