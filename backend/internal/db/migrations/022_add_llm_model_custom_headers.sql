SET @llm_model_custom_headers_column_exists = (
  SELECT COUNT(*)
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'llm_models'
    AND COLUMN_NAME = 'custom_headers_json'
);
SET @llm_model_custom_headers_column_sql = IF(
  @llm_model_custom_headers_column_exists = 0,
  'ALTER TABLE llm_models ADD COLUMN custom_headers_json LONGTEXT NULL AFTER api_key_secret_ref',
  'SELECT 1'
);
PREPARE llm_model_custom_headers_column_stmt FROM @llm_model_custom_headers_column_sql;
EXECUTE llm_model_custom_headers_column_stmt;
DEALLOCATE PREPARE llm_model_custom_headers_column_stmt;
