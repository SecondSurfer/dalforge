# Version History

## v1.5.5 - 2026-10-01
  - change: explicitly set Created or Updated on an entity will be used. Currently it would always default to now

## v1.5.4 - 2026-04-18
  - change: desc -> descending renamed. Dropped desc

## v1.5.3 - 2026-04-15
  - new: epoch list invalidation for caching

## v1.5.2 - 2026-04-15
  - new: pluck operations
  - improvement: better sql indexes created
  - fix: validate collision with function names
  - fix: dont allow listBulk operation with unique keys in whereIn

## v1.5.1 - 2026-04-11
  - performance fix: bulk delete has limit of 5000 items. to delete everything needs to use for loop in calls.
  - new: bulk list feature

## v1.5.0 - 2026-04-10
  - added bulk gets and bulk update functionality

## v1.4.0 - 2026-04-06
  - removed Store methods as they can cause a lot of problems in distributed setup; for entities with multiple unique keys.

## v1.3.6 - 2026-04-03 
  - fixed bug to allow created/updated columns in where of lists

## v1.3.5 - 2026-03-30
  - deletes section in yaml for allowing bulk deletes

## v1.3.4 - 2026-03-28
  - softDelete feature added to dal layer

## v1.3.3 - 2026-03-27
  - fixed dsn connection string to use &loc=UTC for db connections.
  - fixed infof to debugf for cache invalidations.
  - configuration for server group support "all" value to allow any entity name.
  - increased length of uid from 50 to 255 chars
  - fixed bug in createtable when running sql statements with comments 
  - closing redis will nicely close pubsub connections, too.
  - circuit breaker now ignores ErrNotFound error type

## v1.3.0 - 2026-03-17
  - added support for json field type

## v1.2.0 - 2026-03-09
  - added typeMapping instructions for where clauses. so definig where: user1 = :user OR user2 = :user is possible

## v1.1.0 - 2026-02-24
 - renamed DAL to Repository structs
 - improved various small bugs


## v1.0.0 - first release