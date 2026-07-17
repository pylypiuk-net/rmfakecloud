// Jenkins Pipeline — rmfakecloud (forked from ddvk/rmfakecloud)
// Multi-stage Dockerfile: Node UI build → Go build → scratch image
// Kaniko builds and pushes to Gitea container registry

pipeline {
    agent {
        kubernetes {
            yamlFile 'builder.yaml'
        }
    }

    environment {
        REGISTRY = 'vcs.int.pylypiuk.net'
    }

    options {
        ansiColor('xterm')
        timeout(time: 30, unit: 'MINUTES')
        buildDiscarder(logRotator(numToKeepStr: '20'))
        disableConcurrentBuilds()
    }

    stages {
        stage('Checkout') {
            steps {
                checkout scm
                sh 'git rev-parse --short HEAD > commit.txt'
                script {
                    env.GIT_COMMIT_SHORT = readFile('commit.txt').trim()
                    echo "Building commit ${env.GIT_COMMIT_SHORT}"
                }
            }
        }

        stage('Docker Build & Push') {
            when {
                anyOf {
                    branch 'main'
                    branch 'master'
                }
            }
            parallel {
                stage('Build rmfakecloud') {
                    steps {
                        container('kaniko') {
                            sh '''
                            /kaniko/executor --dockerfile `pwd`/Dockerfile \
                                             --context `pwd` \
                                             --destination=${REGISTRY}/jenkins/rmfakecloud:v0.${BUILD_NUMBER} \
                                             --destination=${REGISTRY}/jenkins/rmfakecloud:latest \
                                             --skip-tls-verify
                            '''
                        }
                    }
                }
                stage('Build rm-ingestion') {
                    steps {
                        container('kaniko') {
                            catchError(buildResult: 'UNSTABLE', stageResult: 'UNSTABLE') {
                                sh '''
                                /kaniko/executor --dockerfile `pwd`/docker/ingestion/Dockerfile \
                                                 --context `pwd`/docker/ingestion \
                                                 --destination=${REGISTRY}/jenkins/rm-ingestion:v0.${BUILD_NUMBER} \
                                                 --destination=${REGISTRY}/jenkins/rm-ingestion:latest \
                                                 --skip-tls-verify
                                '''
                            }
                        }
                    }
                }
            }
        }
    }

    post {
        success {
            echo "Pipeline SUCCESS — commit ${env.GIT_COMMIT_SHORT ?: 'unknown'}"
        }
        failure {
            echo "Pipeline FAILED — commit ${env.GIT_COMMIT_SHORT ?: 'unknown'}"
        }
    }
}