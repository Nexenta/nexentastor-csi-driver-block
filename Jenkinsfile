pipeline {
    parameters {
        string(
            name: 'TEST_K8S_IP',
            defaultValue: '10.3.132.116',
            description: 'K8s setup IP address to test on',
            trim: true
        )
    }
    options {
        disableConcurrentBuilds()
    }
    agent {
        node {
            label 'solutions-126'
        }
    }
    environment {
        TESTRAIL_URL = 'https://testrail.nexenta.com/testrail'
        TESTRAIL = credentials('solutions-napalm')
    }
    stages {
        stage('Build') {
            steps {
                sh 'make container-build'
            }
        }
        stage('Tests [unit]') {
            steps {
                echo "here it will be unit tests"
            }
        }
        stage('Tests [csi-sanity]') {
            steps {
                sh 'make test-csi-sanity-container'
            }
        }
        stage('Push [local registry]') {
            steps {
                sh 'make container-push-local'
            }
        }
        stage('Tests [local registry]') {
            steps {
                sh "TEST_K8S_IP=${params.TEST_K8S_IP} TESTRAIL_URL=${TESTRAIL_URL} TESTRAIL_USR=${TESTRAIL_USR} TESTRAIL_PSWD=${TESTRAIL_PSW} make test-e2e-k8s-local-image-container"
            }
        }
        stage('Push [hub.docker.com]') {
            when {
                anyOf {
                    branch 'master'
                    branch pattern: '\\d\\.\\d\\.\\d', comparator: 'REGEXP'
                }
            }
            environment {
                DOCKER = credentials('docker-hub-credentials')
            }
            steps {
                sh '''
                    docker login -u ${DOCKER_USR} -p ${DOCKER_PSW};
                    make container-push-remote;
                '''
            }
        }
        stage('Tests [k8s hub.docker.com]') {
            when {
                anyOf {
                    branch 'master'
                    branch pattern: '\\d\\.\\d\\.\\d', comparator: 'REGEXP'
                }
            }
            steps {
                sh "TEST_K8S_IP=${params.TEST_K8S_IP} TESTRAIL_URL=${TESTRAIL_URL} TESTRAIL_USR=${TESTRAIL_USR} TESTRAIL_PSWD=${TESTRAIL_PSW} make test-e2e-k8s-remote-image-container"
            }
        }
    }
    post {
        success {
            office365ConnectorSend webhookUrl: "https://tintrivmstore.webhook.office.com/webhookb2/712fb1a6-6ff1-4fba-ad91-7ea7a01a3839@7aa633be-c8f9-43fe-aff7-41aa956c6e9e/JenkinsCI/c7567f6ab90e432cbc44876e80a0fb24/dcb3f841-f28e-4e68-adf6-edc9d1175286"
        }
	failure {
            office365ConnectorSend webhookUrl: "https://tintrivmstore.webhook.office.com/webhookb2/712fb1a6-6ff1-4fba-ad91-7ea7a01a3839@7aa633be-c8f9-43fe-aff7-41aa956c6e9e/JenkinsCI/c7567f6ab90e432cbc44876e80a0fb24/dcb3f841-f28e-4e68-adf6-edc9d1175286"
        }
	aborted {
            office365ConnectorSend webhookUrl: "https://tintrivmstore.webhook.office.com/webhookb2/712fb1a6-6ff1-4fba-ad91-7ea7a01a3839@7aa633be-c8f9-43fe-aff7-41aa956c6e9e/JenkinsCI/c7567f6ab90e432cbc44876e80a0fb24/dcb3f841-f28e-4e68-adf6-edc9d1175286"
	}
    }
}
