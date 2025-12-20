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
                    // Running Real Tests (Integration Mode)
                    // This generates the cucumber.json from the actual .feature files in the workspace.
                    script {
                        try {
                            sh "behave -f json.pretty -o cucumber.json features/"
                        } catch (Exception e) {
                            echo "Behave tests failed (some scenarios failed). Continuing pipeline to ingest results."
                            // We don't error out because Vordu wants to visualize the failure.
                        }
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
