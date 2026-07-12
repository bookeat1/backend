package auth

// Input DTOs for the auth endpoints. Binding tags drive Gin's validator.

type signupRequest struct {
	Email    string `json:"email" binding:"required,email" example:"user@example.com"`
	Password string `json:"password" binding:"required,min=6" example:"s3cret123"`
	FullName string `json:"full_name" example:"Jane Doe"`
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email" example:"user@example.com"`
	Password string `json:"password" binding:"required" example:"s3cret123"`
}

type otpRequestRequest struct {
	Phone string `json:"phone" binding:"required" example:"+77011234567"`
}

type otpVerifyRequest struct {
	Phone string `json:"phone" binding:"required" example:"+77011234567"`
	Code  string `json:"code" binding:"required" example:"123456"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required" example:"9c8b7a6f-...-refresh"`
}
