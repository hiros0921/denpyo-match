require_relative "boot"

require "rails"
# Pick the frameworks you want:
require "active_model/railtie"
require "active_job/railtie"
require "active_record/railtie"
# require "active_storage/engine"
require "action_controller/railtie"
require "action_mailer/railtie"
# require "action_mailbox/engine"
# require "action_text/engine"
require "action_view/railtie"
# require "action_cable/engine"
# require "rails/test_unit/railtie"

# Require the gems listed in Gemfile, including any gems
# you've limited to :test, :development, or :production.
Bundler.require(*Rails.groups)

module DenpoMatch
  class Application < Rails::Application
    # Initialize configuration defaults for originally generated Rails version.
    config.load_defaults 8.1

    # Please, add to the `ignore` list any other `lib` subdirectories that do
    # not contain `.rb` files, or that should not be reloaded or eager loaded.
    # Common ones are `templates`, `generators`, or `middleware`, for example.
    config.autoload_lib(ignore: %w[assets tasks])

    # Configuration for the application, engines, and railties goes here.
    #
    # These settings can be overridden in specific environments using the files
    # in config/environments, which are processed later.
    #
    # config.time_zone = "Central Time (US & Canada)"
    # config.eager_load_paths << Rails.root.join("extras")

    # Don't generate system test files.
    config.generators.system_tests = nil

    # ── このアプリの前提 ──
    #
    # スキーマの正は db/migrations/*.sql。Rails は構造を管理しない。
    # 同じテーブルを Go と Rails の両方から触るため、片方に管理を任せると
    # もう片方から見て「勝手に変わる」ことになる。
    config.active_record.schema_format = :sql
    # マイグレーションを持たないので、保留チェックも要らない。
    # 有効なままだと、SQLで足した列を Rails が「未適用の変更」と誤認する。
    config.active_record.migration_error = false

    config.time_zone = "Tokyo"
    # DBには UTC で入れる。表示するときだけ日本時間へ直す。
    # 監査ログを扱うので、保存側の時刻がロケールで揺れる状態は作らない。
    config.active_record.default_timezone = :utc

    config.i18n.default_locale = :ja
    config.i18n.available_locales = [:ja, :en]

    # 業務時間中の事務所で使う。既定のままだと英語の例外画面が出る。
    config.action_view.field_error_proc = proc { |html_tag, _| html_tag }
  end
end
