package models

// TeamGroupGrant lets every key of a Casdoor team route through one group.
type TeamGroupGrant struct {
	TeamID      string `gorm:"type:varchar(255);primaryKey"`
	GroupID     uint   `gorm:"primaryKey"`
	CreatedAtMS int64  `gorm:"column:created_at_ms;not null;autoCreateTime:milli"`
}
