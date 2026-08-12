Rails.application.routes.draw do
  # 自分でアカウントを作る経路は塞ぐ。理由は app/models/user.rb に書いた。
  # モデルから :registerable を外すだけだと、ルートは残ったままになる。
  devise_for :users, skip: [:registrations]

  root "documents#index"

  resources :documents, only: [:index, :new, :create, :show] do
    member do
      # 人の判断。承認・修正・却下。
      post :decide
    end
  end

  # 承認キュー。要確認と却下が並ぶ。事務所が一番長く見る画面。
  resources :reviews, only: [:index]

  # 閾値シミュレーション。このプロダクトの核心。
  resources :thresholds, only: [:index, :create] do
    collection { get :simulate }
  end

  # 覚えた表記。人が承認時に教えたもの。見て取り消せる。
  resources :learned_aliases, only: [:index, :destroy]

  # 契約。申し込みと、支払い方法の変更。
  resource :billing, only: [:show] do
    post :checkout
    post :portal
  end

  # 誤認識の影響範囲。1件の誤りが他に及んでいないかを調べる。
  resources :impacts, only: [:index]

  # 入出金との突合。受領した伝票に対応する支払いがあったかを確かめる。
  resources :settlements, only: [:index] do
    collection do
      post :import   # 明細CSVの取り込み
      post :run      # 突合の再実行
    end
    member { post :confirm }
  end

  # インボイス登録番号の一括照合。受領側でしか発生しない作業。
  resources :invoice_regs, only: [:index]

  # 監査ログ。読むだけ。
  resources :audit_logs, only: [:index]

  get "up" => "rails/health#show", as: :rails_health_check
end
