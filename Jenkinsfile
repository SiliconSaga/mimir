@Library('vordu-lib') _

pipeline {
    agent any

    environment {
        // Pointing to local Dev Vörðu API for prototype
        VORDU_API_URL = 'http://vordu-api:8000'
        VORDU_API_KEY = 'dev-key'
    }

    stages {
        stage('Checkout') {
            steps {
                script {
                    echo "Checking out Mimir..."
                    // git checkout logic would go here
                    // For prototype, we assume workspace is mounted
                }
            }
        }
        
        stage('Test Infrastructure') {
            steps {
                script {
                    echo "Running BDD Infrastructure Tests..."
                    // Real: sh 'behave features/'
                    // Prototype: Generate a mock report since we don't have K8s/Behave env yet
                    
                    def mockReport = [
                        [
                             uri: 'features/kafka.feature',
                             elements: [
                                 [
                                     type: 'scenario',
                                     tags: [[name: '@component:mimir-kafka'], [name: '@phase:1']],
                                     steps: [[result: [status: 'passed']]]
                                 ]
                             ]
                        ]
                    ]
                    
                    writeFile file: 'cucumber.json', text: groovy.json.JsonOutput.toJson(mockReport)
                }
            }
        }

        stage('Ingest to Vörðu') {
            steps {
                script {
                    // Call the Shared Library Step
                    ingestVordu(
                        catalogPath: 'catalog-info.yaml', // Mimir's catalog (needs to exist!)
                        reportPath: 'cucumber.json',
                        apiUrl: 'http://localhost:8000' // Using localhost for local verification
                    )
                }
            }
        }
    }
}
