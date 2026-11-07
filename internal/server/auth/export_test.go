package auth

// WaitAPITokenUse 等待后台的最后使用时间写入完成。
func (s *Service) WaitAPITokenUse() { s.tokenUses.Wait() }

func (s *Service) APITokenBucketCount() int {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	return len(s.apiTokenBuckets)
}
