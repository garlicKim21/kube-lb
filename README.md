# Kube-LB

Kubernetes 기반 L4 로드밸런서 구현체. LB 클러스터는 Cilium을 사용해 VIP를 생성하고 트래픽을 컨슈머 클러스터로 라우팅하며, 컨슈머 클러스터는 CNI 종속성을 제거합니다.

## 디렉터리 구조

- `cmd/webhook/`: LB 클러스터의 Webhook 서버
- `cmd/agent/`: 컨슈머 클러스터의 에이전트
- `pkg/api/`: Webhook 요청/응답 구조체
- `pkg/webhook/`: Webhook 서버 로직
- `pkg/agent/`: 에이전트 로직
- `pkg/cilium/`: Cilium 유틸리티
- `deploy/`: Kubernetes 매니페스트

## 다음 단계

1. Webhook 서버에 VIP 할당 로직 추가
2. Cilium eBPF 라우팅 설정
3. 에이전트 구현