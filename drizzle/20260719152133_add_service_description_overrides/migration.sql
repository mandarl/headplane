CREATE TABLE `service_description_overrides` (
	`id` text PRIMARY KEY,
	`host_id` text NOT NULL,
	`proto` text NOT NULL,
	`port` integer NOT NULL,
	`description` text NOT NULL,
	`updated_by` text,
	`updated_at` integer
);
