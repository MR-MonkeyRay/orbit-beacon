package auth

// WaitAPITokenUse 等待后台的最后使用时间写入完成。
func (s *Service) WaitAPITokenUse() { s.tokenUses.Wait() }
