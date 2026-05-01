
## 작업 디렉터리 규칙

`.worktree/` 폴더는 git worktree 및 보조 clone 들을 모아두는 위치입니다. `.gitignore` 처리되어 있으니 자유롭게 사용해도 됩니다.

### GitHub Wiki 작업

이 저장소의 위키(`https://github.com/egoavara/route-prism.wiki.git`)를 수정해야 할 때는 항상 `.worktree/.wiki/` 에서 작업합니다.

- 처음 작업 시 한 번만 clone:
  ```bash
  mkdir -p .worktree
  git clone https://github.com/egoavara/route-prism.wiki.git .worktree/.wiki
  ```
- 이후 모든 위키 편집·커밋·푸시는 `.worktree/.wiki/` 안에서 수행합니다.
- 위키는 별도 저장소이므로 본 저장소 커밋과 절대 섞지 않습니다.

### 일반 git worktree

기능 브랜치를 격리된 트리에서 만지고 싶을 때도 `.worktree/<branch-name>` 패턴으로 둡니다:
```bash
git worktree add .worktree/<branch-name> <branch-name>
```

## 사용자별 설정

@CLAUDE.local.md