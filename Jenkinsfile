pipeline {
    agent any

    environment {
        IMAGE_NAME = "erp-inventory-service"
    }

    stages {
        stage('Checkout') {
            steps {
                checkout scm
            }
        }
        stage('Vet & Race-Checked Tests') {
            steps {
                sh 'go vet ./... && go test -v -race ./...'
            }
        }
        stage('Build Docker Image') {
            steps {
                sh "docker build -t ${IMAGE_NAME}:${BUILD_NUMBER} ."
            }
        }
    }
}