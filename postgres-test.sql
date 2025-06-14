-- PostgreSQL test script
-- Run with: psql -f postgres-test.sql

-- Create a sample table
CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    email VARCHAR(100) UNIQUE NOT NULL,
    age INTEGER,
    roles TEXT[],
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Insert sample data
INSERT INTO users (name, email, age, roles) VALUES
    ('John Doe', 'john@example.com', 30, ARRAY['user', 'admin']),
    ('Jane Smith', 'jane@example.com', 25, ARRAY['user'])
ON CONFLICT (email) DO NOTHING;

-- Create an index on the email field
CREATE INDEX IF NOT EXISTS users_email_idx ON users(email);

-- Verify the data
SELECT * FROM users;

-- Test a query
SELECT * FROM users WHERE 'admin' = ANY(roles);

-- Test aggregation
SELECT 
    unnest(roles) as role,
    AVG(age) as avg_age
FROM users
GROUP BY unnest(roles); 