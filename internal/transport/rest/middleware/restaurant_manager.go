package middleware

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
)

// RestaurantManagerChecker answers whether a user manages a restaurant.
// Implemented by usecase/restaurants.ManagerUseCase.
type RestaurantManagerChecker interface {
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
}

// RequireRestaurantManager gates a route to admins and to restaurant-role users
// who manage the restaurant identified by the path parameter named param. Must
// run after Auth. Admins always pass. A restaurant-role user passes iff
// Manages(userID, restaurantID) is true. Everyone else is forbidden.
func RequireRestaurantManager(mc RestaurantManagerChecker, param string) gin.HandlerFunc {
	return func(c *gin.Context) {
		au, ok := GetAuthUser(c.Request.Context())
		if !ok {
			response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
			c.Abort()
			return
		}
		if au.Role == string(domain.RoleAdmin) {
			c.Next()
			return
		}
		if au.Role != string(domain.RoleRestaurant) {
			response.Error(c.Writer, http.StatusForbidden, "forbidden")
			c.Abort()
			return
		}
		rid, err := uuid.Parse(c.Param(param))
		if err != nil {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
			c.Abort()
			return
		}
		manages, err := mc.Manages(c.Request.Context(), au.ID, rid)
		if err != nil {
			response.HandleError(c.Writer, err)
			c.Abort()
			return
		}
		if !manages {
			response.Error(c.Writer, http.StatusForbidden, "forbidden")
			c.Abort()
			return
		}
		c.Next()
	}
}
