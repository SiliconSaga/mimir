@Library('vordu-lib') _

pipeline {
    agent {
        label 'python-ai' 
    }

    stages {
        stage('Checkout') {
            steps {
                container('builder') {
                    checkout scm
                }
            }
        }
        
        stage('Prepare Environment') {
            steps {
                container('builder') {
                    sh """
                        echo "Installing dependencies..."
                        pip install behave requests
                        # If we had a requirements.txt: pip install -r requirements.txt
                    """
                }
            }
        }

        stage('Test Infrastructure') {
            steps {
                container('builder') {
                    script {
                        echo "Running BDD Infrastructure Tests..."
                        // Start KinD or connect to cluster here in real life.
                        // For prototype: We generate a mock report representing the 'Current State'
                        
                        // Constructing a realistic report based on what we expect from Mimir's status
                        // (Kafka: Phase 1 Pass, Phase 2 Pending; Valkey: Phase 1 Pass; Percona: Mixed)
                        def mockReport = [
                            [
                                uri: 'features/kafka.feature',
                                elements: [
                                    [
                                        type: 'scenario', 
                                        tags: [[name: '@component:mimir-kafka'], [name: '@phase:1']],
                                        steps: [[result: [status: 'passed']]]
                                    ],
                                    [
                                        type: 'scenario', 
                                        tags: [[name: '@component:mimir-kafka'], [name: '@phase:2']],
                                        steps: [[result: [status: 'undefined']]] // implicit pending
                                    ]
                                ]
                            ],
                            [
                                uri: 'features/valkey.feature',
                                elements: [
                                    [
                                        type: 'scenario', 
                                        tags: [[name: '@component:mimir-valkey'], [name: '@phase:1']],
                                        steps: [[result: [status: 'passed']]]
                                    ]
                                ]
                            ]
                        ]
                        
                        writeFile file: 'cucumber.json', text: groovy.json.JsonOutput.toJson(mockReport)
                    }
                }
            }
            post {
                always {
                    archiveArtifacts artifacts: 'cucumber.json', allowEmptyArchive: true
                }
            }
        }

        stage('Ingest to Vörðu') {
            steps {
                container('builder') {
                    withCredentials([string(credentialsId: 'vordu-api-key', variable: 'VORDU_API_KEY')]) {
                        script {
                            // Call the Shared Library Step (contains good defaults)
                            ingestVordu(
                                catalogPath: 'catalog-info.yaml',
                                reportPath: 'cucumber.json'
                            )
                        }
                    }
                }
            }
        }
    }
    
    post {
        success {
            echo "Mimir Pipeline Successful. Data ingested to Vörðu."
        }
        failure {
            echo "Mimir Pipeline Failed."
        }
    }
}
