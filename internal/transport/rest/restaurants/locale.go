package restaurants

import (
	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/reqlocale"
)

// resolveLocale is a thin alias for reqlocale.Resolve — see that package for
// the full resolution rules (query param, Accept-Language, fallback to ru).
func resolveLocale(c *gin.Context) string { return reqlocale.Resolve(c) }
