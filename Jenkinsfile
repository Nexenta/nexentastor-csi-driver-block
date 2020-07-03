pipeline {
    parameters {
        string(
            name: 'TEST_K8S_IP',
            defaultValue: '10.3.199.174',
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
                echo "here it will be csi sanity tests"
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
                branch 'master'
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
                branch 'master'
            }
            steps {
                sh "TEST_K8S_IP=${params.TEST_K8S_IP} TESTRAIL_URL=${TESTRAIL_URL} TESTRAIL_USR=${TESTRAIL_USR} TESTRAIL_PSWD=${TESTRAIL_PSW} make test-e2e-k8s-remote-image-container"
            }
        }
    }
}
