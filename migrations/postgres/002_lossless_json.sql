-- JSONB's numeric range and Unicode restrictions must not constrain opaque DSC
-- JSON stored as text. JSON validates syntax/shape without numeric conversion.
ALTER TABLE configurations DROP CONSTRAINT configurations_template_json_check;
ALTER TABLE configurations DROP CONSTRAINT configurations_parameters_json_check;
ALTER TABLE configurations ADD CONSTRAINT configurations_template_json_check
    CHECK (json_typeof(template_json::json) = 'object');
ALTER TABLE configurations ADD CONSTRAINT configurations_parameters_json_check
    CHECK (parameters_json IS NULL OR json_typeof(parameters_json::json) = 'object');
