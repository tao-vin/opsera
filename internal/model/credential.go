package model

type Credential struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	SecretCipher string `json:"secretCipher"`
}
