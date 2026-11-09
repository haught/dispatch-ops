package store

// LoginLockKeys are the advisory lock keys ReserveLoginAttempt takes for
// username and clientIP, for tests that hold them.
func LoginLockKeys(username, clientIP string) (user, ip int64) {
	return loginLockKey("user", loginUsernameHash(username)), loginLockKey("ip", []byte(LoginClientKey(clientIP)))
}

// LoginLockoutKey is the login_lockouts key of limit for username from
// clientIP.
func LoginLockoutKey(limit LoginLimit, username, clientIP string) []byte {
	k := loginLockoutKey(limit, loginUsernameHash(username), LoginClientKey(clientIP))
	return k[:]
}
