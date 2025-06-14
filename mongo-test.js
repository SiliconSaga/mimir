// MongoDB test script
// Run with: mongosh --file mongo-test.js

// Use the admin database where we have permissions
db = db.getSiblingDB('admin');

// Drop the test collection if it exists
print('\nCleaning up any existing test data...');
db.test_users.drop();

// Create a test collection
print('\nCreating test collection...');
db.createCollection('test_users');

// Insert sample documents
print('\nInserting test data...');
db.test_users.insertMany([
  {
    name: 'John Doe',
    email: 'john@example.com',
    age: 30,
    roles: ['user', 'admin'],
    created_at: new Date()
  },
  {
    name: 'Jane Smith',
    email: 'jane@example.com',
    age: 25,
    roles: ['user'],
    created_at: new Date()
  }
]);

// Create an index on the email field
print('\nCreating index...');
db.test_users.createIndex({ email: 1 }, { unique: true });

// Verify the data
print('\nAll users:');
db.test_users.find().pretty();

// Test a query
print('\nUsers with admin role:');
db.test_users.find({ roles: 'admin' }).pretty();

// Test aggregation
print('\nAverage age by role:');
db.test_users.aggregate([
  { $unwind: '$roles' },
  { $group: { _id: '$roles', avgAge: { $avg: '$age' } } }
]).pretty();

print('\nTest completed successfully!'); 