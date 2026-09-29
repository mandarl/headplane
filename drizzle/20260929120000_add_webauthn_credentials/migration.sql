CREATE TABLE `webauthn_credentials` (
	`id` text PRIMARY KEY NOT NULL,
	`user_id` text NOT NULL,
	`credential_id` text NOT NULL,
	`public_key` blob NOT NULL,
	`attestation_type` text DEFAULT 'none' NOT NULL,
	`transports` text,
	`backup_eligible` integer DEFAULT 0 NOT NULL,
	`backup_state` integer DEFAULT 0 NOT NULL,
	`label` text,
	`sign_count` integer DEFAULT 0 NOT NULL,
	`created_at` integer,
	`updated_at` integer,
	`last_used_at` integer,
	FOREIGN KEY (`user_id`) REFERENCES `users`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `webauthn_credentials_credential_id_unique` ON `webauthn_credentials` (`credential_id`);
--> statement-breakpoint
CREATE INDEX `webauthn_credentials_user_id_idx` ON `webauthn_credentials` (`user_id`);
